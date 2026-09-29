package basispoints

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
)

// headerOnlyPNG 生成只含签名与 IHDR 的 PNG。image.DecodeConfig 只读图片头，
// 因此可以用极小体积断言超大像素尺寸，而不必真的编码 64MP 以上的位图。
func headerOnlyPNG(width, height int) []byte {
	var buffer bytes.Buffer
	buffer.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := []byte{'I', 'H', 'D', 'R'}
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(width))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(height))
	ihdr = append(ihdr, 8, 2, 0, 0, 0)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)-4))
	chunk = append(chunk, ihdr...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(ihdr))
	buffer.Write(chunk)
	return buffer.Bytes()
}

// paddedPNGDataURL 在合法 PNG 头之后追加 padding 个零字节，用于体积受限用例。
func paddedPNGDataURL(width, height, padding int) string {
	raw := headerOnlyPNG(width, height)
	raw = append(raw, make([]byte, padding)...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func contentBody(parts ...any) map[string]any {
	return map[string]any{"input": []any{map[string]any{"role": "user", "content": parts}}}
}

func imagePart(dataURL string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": dataURL}
}

func TestInlineImagePreflightRejectsBeforeAnyUpload(t *testing.T) {
	tiny, _ := testImageDataURL(t)
	oversized := paddedPNGDataURL(3, 2, maxInlineImageBytes)
	seventeenMiB := paddedPNGDataURL(3, 2, 17<<20)
	tooManyPixels := paddedPNGDataURL(100000, 1000, 0)
	// 声明值不再参与上传格式判定（字节签名说了算），所以这里只保留真正会被拒的用例。
	undecodable := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image"))
	limitParts := make([]any, 0, defaultMaxRequestInlineImages+1)
	for range defaultMaxRequestInlineImages + 1 {
		limitParts = append(limitParts, imagePart(tiny))
	}
	for _, tc := range []struct {
		name  string
		parts []any
	}{
		{"count", limitParts},
		{"single-image-size", []any{imagePart(oversized)}},
		{"request-total-size", []any{imagePart(seventeenMiB), imagePart(seventeenMiB)}},
		{"pixel-count", []any{imagePart(tooManyPixels)}},
		{"undecodable", []any{imagePart(undecodable)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService()
			uploads := 0
			service.SetHost(func(string, any, any) error {
				uploads++
				return errors.New("preflight rejection must not upload")
			})
			err := service.uploadInputImages(ExecutorRequest{}, contentBody(tc.parts...), credential{}, defaultConfig())
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 400 {
				t.Fatalf("err=%v, want 400", err)
			}
			if uploads != 0 {
				t.Fatalf("uploads=%d, want 0 (preflight must run before any upload)", uploads)
			}
		})
	}
}

func TestInlineImageDetailRetainedAndValidated(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	for _, tc := range []struct {
		name   string
		detail any
		want   any
		ok     bool
	}{
		{"missing", nil, "auto", true},
		{"auto", "auto", "auto", true},
		{"low", "low", "low", true},
		{"high", "high", "high", true},
		{"original", "original", "original", true},
		{"invalid-enum", "ultra", nil, false},
		{"non-string", 7, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part := imagePart(dataURL)
			if tc.detail != nil {
				part["detail"] = tc.detail
			}
			body := contentBody(part)
			service := NewService()
			uploads := 0
			service.SetHost(func(_ string, _ any, out any) error {
				uploads++
				*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-detail"})}
				return nil
			})
			err := service.uploadInputImages(ExecutorRequest{}, body, credential{}, defaultConfig())
			if !tc.ok {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 400 || uploads != 0 {
					t.Fatalf("invalid detail accepted: err=%v uploads=%d", err, uploads)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := objectValue(objectValue(body["input"].([]any)[0])["content"].([]any)[0])
			if got["detail"] != tc.want || got["file_id"] != "file-detail" || got["image_url"] != nil {
				t.Fatalf("unexpected rewritten part: %#v", got)
			}
		})
	}
}

func TestValidAttachmentIDBounds(t *testing.T) {
	maxLength := "file-" + strings.Repeat("a", 251)
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"", false},
		{"file-", false},
		{"file-a", true},
		{"file-A_9-z", true},
		{"file-.", false},
		{"file-a b", false},
		{"file-über", false},
		{"files-abc", false},
		{maxLength, true},
		{maxLength + "a", false},
	} {
		if got := validAttachmentID(tc.id); got != tc.want {
			t.Fatalf("validAttachmentID(%q)=%t want %t (len=%d)", tc.id, got, tc.want, len(tc.id))
		}
	}
}

func TestUploadRejectsMalformedAttachmentID(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	for _, id := range []any{"bogus", "file-", "file-bad.id", 123, "  "} {
		service := NewService()
		uploads := 0
		service.SetHost(func(_ string, _ any, out any) error {
			uploads++
			*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": id})}
			return nil
		})
		_, _, err := service.prepareRequest(imageRequest(imagePart(dataURL)))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 502 || uploads != 1 {
			t.Fatalf("openai_file_id %#v was accepted: err=%v uploads=%d", id, err, uploads)
		}
	}
}

func TestRemoteImageURLsAreNeverUploaded(t *testing.T) {
	source := map[string]any{"input": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "https://example.test/image.png", "detail": "auto"}}},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": []any{map[string]any{"type": "input_image", "image_url": "http://example.test/tool.png"}}},
	}}
	service := NewService()
	uploads := 0
	service.SetHost(func(string, any, any) error {
		uploads++
		return errors.New("remote image URL must not be uploaded")
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	if uploads != 0 {
		t.Fatalf("uploads=%d, want 0", uploads)
	}
	items := source["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("item count = %d, want 3 (the tool reference gains a follower message)", len(items))
	}
	// 消息里的远端引用已在合法位置：原样保留。
	if part := objectValue(objectValue(items[0])["content"].([]any)[0]); part["image_url"] != "https://example.test/image.png" || part["detail"] != "auto" {
		t.Fatalf("message remote reference changed: %#v", part)
	}
	// 工具结果里的远端引用原位换成标签文本，引用本身搬进相邻 user 消息。
	output := objectValue(items[1])["output"].([]any)
	if len(output) != 1 || stringValue(objectValue(output[0])["type"]) != "input_text" {
		t.Fatalf("tool reference was not labeled in place: %#v", output)
	}
	parts := objectValue(items[2])["content"].([]any)
	if len(parts) != 3 || objectValue(parts[2])["image_url"] != "http://example.test/tool.png" {
		t.Fatalf("tool reference was not relocated: %#v", parts)
	}
}

// 诊断计数必须覆盖工具结果的 output，否则日志里的 input_images 会少算。
func TestUpstreamDiagnosticCountsToolOutputImages(t *testing.T) {
	body := map[string]any{"input": []any{
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "input_image", "detail": "original"}}},
		map[string]any{"type": "function_call_output", "output": []any{map[string]any{"type": "input_image"}}},
		map[string]any{"type": "custom_tool_call_output", "output": []any{map[string]any{"type": "input_image"}}},
		map[string]any{"type": "function_call", "content": []any{map[string]any{"type": "input_image"}}},
	}}
	err := upstreamRequestError(500, jsonBytes(map[string]any{"message": "boom"}), body, credential{})
	if !strings.Contains(err.Error(), "input_images=3") || !strings.Contains(err.Error(), "original_detail_images=1") {
		t.Fatalf("diagnostic missed tool output images: %v", err)
	}
}

// 张数上限现在由配置决定：默认 defaultMaxRequestInlineImages，未设置时回落默认，
// 越界由配置校验拒绝。旧值 20 会把长会话锁死，所以默认值必须远大于历史长度。
func TestInlineImageLimitFollowsConfiguration(t *testing.T) {
	tiny, _ := testImageDataURL(t)
	parts := func(n int) []any {
		out := make([]any, 0, n)
		for range n {
			out = append(out, imagePart(tiny))
		}
		return out
	}
	service := NewService()
	service.SetHost(func(_ string, _ any, out any) error {
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-uploaded"})}
		return nil
	})
	if got := defaultConfig().MaxRequestInlineImages; got != defaultMaxRequestInlineImages {
		t.Fatalf("default limit = %d, want %d", got, defaultMaxRequestInlineImages)
	}
	// 未设置（0）时回落默认上限：21 张不再被拒。
	cfg := defaultConfig()
	cfg.MaxRequestInlineImages = 0
	if err := service.uploadInputImages(ExecutorRequest{}, contentBody(parts(21)...), credential{}, cfg); err != nil {
		t.Fatalf("21 images with fallback limit: %v", err)
	}
	// 配置值生效：等于上限放行，超出上限报 400 且带上实际数值。
	cfg.MaxRequestInlineImages = 3
	if err := service.uploadInputImages(ExecutorRequest{}, contentBody(parts(3)...), credential{}, cfg); err != nil {
		t.Fatalf("3 images at limit 3: %v", err)
	}
	err := service.uploadInputImages(ExecutorRequest{}, contentBody(parts(4)...), credential{}, cfg)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Message, "at most 3 inline images") {
		t.Fatalf("err=%v, want 400 mentioning at most 3", err)
	}
}

// 配置校验：0 表示未设置并回落默认；越界（含负数）在加载阶段就以 invalid_config 拒绝。
func TestInlineImageLimitConfigValidation(t *testing.T) {
	cfg := defaultConfig()
	cfg.MaxRequestInlineImages = 0
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize zero limit: %v", err)
	}
	if cfg.MaxRequestInlineImages != defaultMaxRequestInlineImages {
		t.Fatalf("normalized limit = %d, want %d", cfg.MaxRequestInlineImages, defaultMaxRequestInlineImages)
	}
	for _, limit := range []int{-1, maxRequestInlineImagesLimit + 1} {
		invalid := defaultConfig()
		invalid.MaxRequestInlineImages = limit
		err := invalid.normalize()
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Message, "max_request_inline_images") {
			t.Fatalf("limit %d: err=%v, want 400 invalid_config", limit, err)
		}
	}
	bound := defaultConfig()
	bound.MaxRequestInlineImages = maxRequestInlineImagesLimit
	if err := bound.normalize(); err != nil {
		t.Fatalf("normalize upper bound: %v", err)
	}
}
