package basispoints

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
)

// 只缓存摘要和文件 ID，不保存图片或凭据；容量不限制单次请求的图片数量。
const maxAttachmentCacheEntries = 512

// 上传前的整体预检上限：全部校验通过后才开始上传，避免失败时留下孤儿附件。
const (
	maxRequestInlineImages = 20
	maxInlineImageBytes    = 20 << 20
	maxRequestInlineBytes  = 32 << 20
	maxInlineImagePixels   = 64 << 20
)

type cachedAttachment struct {
	key    [sha256.Size]byte
	fileID string
}

type pendingAttachment struct {
	done   chan struct{}
	fileID string
	err    error
}

type attachmentCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*list.Element
	order   list.List
	pending map[[sha256.Size]byte]*pendingAttachment
}

func (c *attachmentCache) getOrUpload(key [sha256.Size]byte, upload func() (string, error)) (string, error) {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.order.MoveToFront(entry)
		fileID := entry.Value.(cachedAttachment).fileID
		c.mu.Unlock()
		return fileID, nil
	}
	if pending := c.pending[key]; pending != nil {
		c.mu.Unlock()
		<-pending.done
		return pending.fileID, pending.err
	}
	if c.pending == nil {
		c.pending = make(map[[sha256.Size]byte]*pendingAttachment)
	}
	pending := &pendingAttachment{done: make(chan struct{})}
	c.pending[key] = pending
	c.mu.Unlock()

	fileID, err := upload()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
	if err == nil {
		if c.entries == nil {
			c.entries = make(map[[sha256.Size]byte]*list.Element)
		}
		c.entries[key] = c.order.PushFront(cachedAttachment{key: key, fileID: fileID})
		if c.order.Len() > maxAttachmentCacheEntries {
			oldest := c.order.Back()
			delete(c.entries, oldest.Value.(cachedAttachment).key)
			c.order.Remove(oldest)
		}
	}
	pending.fileID, pending.err = fileID, err
	close(pending.done)
	return fileID, err
}

type inlineImage struct {
	mediaType string
	data      []byte
}

func decodeInlineImage(dataURL string) (inlineImage, error) {
	metadata, encoded, found := strings.Cut(dataURL[5:], ",")
	if !found {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL is missing its data separator")
	}
	isBase64 := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if isBase64 {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL must declare an image media type")
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL has invalid percent encoding")
	}
	var data []byte
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
	} else {
		data = []byte(decoded)
	}
	if err != nil || len(data) == 0 {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL contains empty or invalid image data")
	}
	return inlineImage{mediaType: mediaType, data: data}, nil
}

func attachmentURL(responsesURL string) (string, error) {
	base, err := url.Parse(strings.TrimRight(responsesURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fail(500, "invalid_config", "cannot derive attachments endpoint from responses_url")
	}
	// 官方附件接口与 responses 同目录、同源，不能把自定义上游的凭据发往其他站点。
	return base.ResolveReference(&url.URL{Path: "attachments"}).String(), nil
}

// inlineImageField 返回条目中可能承载 input_image 的数组字段；空串表示该条目不能包含图片。
func inlineImageField(item map[string]any) string {
	switch stringValue(item["type"]) {
	case "", "message":
		return "content"
	case "function_call_output", "custom_tool_call_output":
		return "output"
	default:
		return ""
	}
}

// validateImageDetail 只接受官方 detail 枚举；缺失时由上传逻辑补 "auto"。
func validateImageDetail(value any) error {
	if value == nil {
		return nil
	}
	switch stringValue(value) {
	case "auto", "low", "high", "original":
		return nil
	default:
		return fail(400, "invalid_image", "input_image detail must be one of auto, low, high, original")
	}
}

// validateInlineImage 只读图片头校验体积、格式与像素，不做完整解码以免内存放大。
// 标准库只注册了 png/jpeg/gif 三种解码器：声明了没有解码器的格式（例如 webp）时读不出图片头，
// 这种情况保留体积与 base64 校验、跳过格式与像素校验，不因为本插件认不出格式就判废请求；
// 声明了有解码器的类型却读不出头，仍然按"内容与声明不符"拒绝。
var decodedImageFormats = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
}

func validateInlineImage(attachment inlineImage) error {
	if len(attachment.data) > maxInlineImageBytes {
		return fail(400, "invalid_image", "input_image exceeds the 20 MiB decoded size limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(attachment.data))
	if err != nil {
		if err == image.ErrFormat && !decodedImageFormats[attachment.mediaType] {
			return nil
		}
		return fail(400, "invalid_image", "input_image data is not a decodable image")
	}
	if "image/"+format != attachment.mediaType {
		return fail(400, "invalid_image", "input_image data does not match its declared media type")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxInlineImagePixels {
		return fail(400, "invalid_image", "input_image dimensions exceed the 64 megapixel limit")
	}
	return nil
}

type pendingImageUpload struct {
	item  int
	field string
	index int
	part  map[string]any
	image inlineImage
}

// uploadInputImages 先对请求里所有内联图片做一次整体校验（张数、单图体积、累计体积、像素），
// 只有全部通过才逐个上传，避免中途失败留下已上传的孤儿附件。
// 用户消息与工具结果（function_call_output/custom_tool_call_output）的数组字段参与处理，
// assistant 消息与不带 data: 前缀的 HTTPS URL 保持原样。
func (s *Service) uploadInputImages(request ExecutorRequest, body map[string]any, c credential, cfg Config) error {
	items, _ := body["input"].([]any)
	var pending []pendingImageUpload
	totalBytes := 0
	for i, value := range items {
		item := objectValue(value)
		if item == nil {
			continue
		}
		field := inlineImageField(item)
		if field == "" {
			continue
		}
		// assistant 历史消息里的图片保持原样：上游不接受 assistant 消息内的 input_image，
		// 只处理用户/系统消息与工具结果。
		if field == "content" && stringValue(item["role"]) == "assistant" {
			continue
		}
		parts, _ := item[field].([]any)
		for j, value := range parts {
			part := objectValue(value)
			if part == nil {
				continue
			}
			imageURL := stringValue(part["image_url"])
			if stringValue(part["type"]) != "input_image" || len(imageURL) < 5 || !strings.EqualFold(imageURL[:5], "data:") {
				continue
			}
			if stringValue(part["file_id"]) != "" {
				return fail(400, "invalid_image", "input_image cannot contain both image_url and file_id")
			}
			if err := validateImageDetail(part["detail"]); err != nil {
				return err
			}
			if len(pending) >= maxRequestInlineImages {
				return fail(400, "invalid_image", fmt.Sprintf("a request may contain at most %d inline images", maxRequestInlineImages))
			}
			attachment, err := decodeInlineImage(imageURL)
			if err != nil {
				return err
			}
			totalBytes += len(attachment.data)
			if totalBytes > maxRequestInlineBytes {
				return fail(400, "invalid_image", "inline images exceed the 32 MiB per-request limit")
			}
			if err := validateInlineImage(attachment); err != nil {
				return err
			}
			pending = append(pending, pendingImageUpload{item: i, field: field, index: j, part: part, image: attachment})
		}
	}
	if len(pending) == 0 {
		return nil
	}
	endpoint, err := attachmentURL(cfg.ResponsesURL)
	if err != nil {
		return err
	}
	updated := make(map[int][]any)
	fields := make(map[int]string)
	for _, upload := range pending {
		hash := sha256.New()
		_, _ = hash.Write(jsonBytes([]string{endpoint, c.AccountID, c.AuthMode, c.AccessToken, upload.image.mediaType}))
		_, _ = hash.Write(upload.image.data)
		var key [sha256.Size]byte
		copy(key[:], hash.Sum(nil))
		fileID, err := s.attachments.getOrUpload(key, func() (string, error) {
			return s.uploadImage(request, endpoint, upload.image, c)
		})
		if err != nil {
			return err
		}
		if updated[upload.item] == nil {
			original, _ := objectValue(items[upload.item])[upload.field].([]any)
			updated[upload.item] = append([]any(nil), original...)
			fields[upload.item] = upload.field
		}
		part := cloneObject(upload.part)
		delete(part, "image_url")
		part["file_id"] = fileID
		if _, exists := part["detail"]; !exists {
			part["detail"] = "auto"
		}
		updated[upload.item][upload.index] = part
	}
	for index, parts := range updated {
		item := cloneObject(objectValue(items[index]))
		item[fields[index]] = parts
		items[index] = item
	}
	return nil
}

func (s *Service) uploadImage(request ExecutorRequest, endpoint string, attachment inlineImage, c credential) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filename := "image"
	if extensions, _ := mime.ExtensionsByType(attachment.mediaType); len(extensions) > 0 {
		filename += extensions[0]
	}
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	partHeaders.Set("Content-Type", attachment.mediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", fail(500, "attachment_encoding", "cannot encode image attachment")
	}
	if _, err := part.Write(attachment.data); err != nil {
		return "", fail(500, "attachment_encoding", "cannot write image attachment")
	}
	if err := writer.Close(); err != nil {
		return "", fail(500, "attachment_encoding", "cannot finish image attachment")
	}
	headers := authHeaders(c, false)
	headers.Set("Content-Type", writer.FormDataContentType())
	var response upstreamResponse
	if err := s.call("host.http.do", map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              endpoint,
		"headers":          headers,
		"body":             body.Bytes(),
	}, &response); err != nil {
		return "", fail(502, "attachment_transport", "Basis Points attachment upload transport failed: "+attachmentErrorMessage([]byte(err.Error()), c, attachment))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fail(response.StatusCode, "attachment_upload_error", fmt.Sprintf("Basis Points attachment upload HTTP %d: %s", response.StatusCode, attachmentErrorMessage(response.Body, c, attachment)))
	}
	var result struct {
		FileID string `json:"openai_file_id"`
	}
	if json.Unmarshal(response.Body, &result) != nil {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned no openai_file_id")
	}
	fileID := strings.TrimSpace(result.FileID)
	if !validAttachmentID(fileID) {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned an invalid openai_file_id")
	}
	return fileID, nil
}

func attachmentErrorMessage(raw []byte, c credential, attachment inlineImage) string {
	message := string(raw)
	for _, secret := range []string{c.AccessToken, c.AccountID, c.Email, base64.StdEncoding.EncodeToString(attachment.data), string(attachment.data)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return redactTokenMessage(errorMessage([]byte(message)))
}

// validAttachmentID 只接受官方附件接口返回的 file_id 形态，拒绝静默透传畸形 ID。
func validAttachmentID(id string) bool {
	if len(id) < 6 || len(id) > 256 || !strings.HasPrefix(id, "file-") {
		return false
	}
	for _, ch := range id[5:] {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}
