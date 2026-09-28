package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

func (s *Service) prepareRequest(request ExecutorRequest) (map[string]any, credential, error) {
	if request.Alt == "responses/compact" {
		return nil, credential{}, fail(400, "unsupported_compaction", "oai-basispoints does not support /responses/compact; send full input history to /responses")
	}
	c, err := credentialFromExecutor(request)
	if err != nil {
		return nil, credential{}, err
	}
	if !c.ExpiresAt.IsZero() && !time.Now().Before(c.ExpiresAt) {
		return nil, credential{}, fail(401, "auth_expired", "ChatGPT OAuth access token has expired")
	}
	source, err := executorSource(request)
	if err != nil {
		return nil, credential{}, err
	}
	cfg := s.config()
	model := stringValue(source["model"])
	if model == "" {
		model = strings.TrimSpace(request.Model)
	}
	source["model"] = model
	source["stream"] = request.Stream
	prepared, err := prepareResponsesBody(source, cfg)
	if err != nil {
		return nil, credential{}, err
	}
	// 先按原始图片计算会话标识，再替换附件引用，避免上传 ID 改变 task/turn。
	if err := s.uploadInputImages(request, prepared, c, cfg); err != nil {
		return nil, credential{}, err
	}
	return prepared, c, nil
}

func authHeaders(c credential, stream bool) http.Header {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	// These headers match the Excel/Basis Points client profile. The access
	// token itself is never logged by this plugin.
	return http.Header{
		"Authorization":           []string{"Bearer " + c.AccessToken},
		"ChatGPT-Account-ID":      []string{c.AccountID},
		"X-OpenAI-Account-ID":     []string{c.AccountID},
		"X-Basispoints-Auth-Mode": []string{c.AuthMode},
		"Content-Type":            []string{"application/json"},
		"Accept":                  []string{accept},
		"Accept-Encoding":         []string{"identity"},
		"Origin":                  []string{"https://bps.openai.com"},
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Editor":         []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Host":           []string{"office"},
		"X-OpenAI-Internal-Basispoints-Client-Platform":       []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": []string{"PC"},
		"X-OpenAI-Internal-Basispoints-Client-Product":        []string{"basispoints-excel-plugin"},
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        []string{"desktop"},
		"X-OpenAI-Internal-Basispoints-Office-Host":           []string{"Excel"},
		"X-OpenAI-Internal-Basispoints-Office-Platform":       []string{"PC"},
		"X-Stainless-Arch":            []string{"unknown"},
		"X-Stainless-Lang":            []string{"js"},
		"X-Stainless-OS":              []string{"Unknown"},
		"X-Stainless-Package-Version": []string{"6.31.0"},
		"X-Stainless-Retry-Count":     []string{"0"},
		"X-Stainless-Runtime":         []string{"browser:chrome"},
		"User-Agent":                  []string{"oai-basispoints/" + Version},
	}
}

func (s *Service) upstreamRequest(request ExecutorRequest, body map[string]any, c credential, stream bool) (upstreamResponse, error) {
	for attempt := 0; ; attempt++ {
		response, err := s.upstreamRequestAttempt(request, body, c, stream, true)
		if isStaleAttachmentOwnership(err) {
			// 缓存的 file_id 已失效：清掉缓存让下一轮重新上传；同一个请求体重试必然再失败。
			s.attachments.reset()
			return response, err
		}
		delay, retry := rateLimitRetryDelay(response.StatusCode, response.Headers, attempt)
		if !retry {
			return response, err
		}
		s.wait(delay)
	}
}

func (s *Service) upstreamRequestAttempt(request ExecutorRequest, body map[string]any, c credential, stream bool, allowEncryptedRetry bool) (upstreamResponse, error) {
	cfg := s.config()
	if cfg.ResponsesURL == "" {
		return upstreamResponse{}, fail(500, "invalid_config", "responses_url is empty")
	}
	payload := map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              cfg.ResponsesURL,
		"headers":          authHeaders(c, stream),
		"body":             jsonBytes(body),
	}
	var response upstreamResponse
	if err := s.call("host.http.do", payload, &response); err != nil {
		return upstreamResponse{}, fail(502, "upstream_transport", "Basis Points transport failed: "+safeError(err))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// 还没有交付任何字节：密文验不过就同路重发一次（同凭据、同请求体其余部分）。
		if allowEncryptedRetry {
			if retryBody, ok := prepareEncryptedContentRetry(body, response.StatusCode, response.Body); ok {
				return s.upstreamRequestAttempt(request, retryBody, c, stream, false)
			}
		}
		return response, upstreamRequestError(response.StatusCode, response.Body, body, c)
	}
	return response, nil
}

func (s *Service) upstreamStream(request ExecutorRequest, body map[string]any, c credential) (upstreamStream, error) {
	for attempt := 0; ; attempt++ {
		stream, err := s.upstreamStreamAttempt(request, body, c, true)
		if isStaleAttachmentOwnership(err) {
			// 缓存的 file_id 已失效：清掉缓存让下一轮重新上传；同一个请求体重试必然再失败。
			s.attachments.reset()
			return stream, err
		}
		delay, retry := rateLimitRetryDelay(stream.StatusCode, stream.Headers, attempt)
		if !retry {
			return stream, err
		}
		s.wait(delay)
	}
}

func (s *Service) upstreamStreamAttempt(request ExecutorRequest, body map[string]any, c credential, allowEncryptedRetry bool) (upstreamStream, error) {
	cfg := s.config()
	payload := map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              cfg.ResponsesURL,
		"headers":          authHeaders(c, true),
		"body":             jsonBytes(body),
	}
	var stream upstreamStream
	if err := s.call("host.http.do_stream", payload, &stream); err != nil {
		return stream, fail(502, "upstream_transport", "Basis Points stream transport failed: "+safeError(err))
	}
	if stream.StreamID == "" {
		return stream, fail(502, "upstream_transport", "host returned no Basis Points stream ID")
	}
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		// 非 2xx 仍有响应流；读取错误原因后关闭，避免丢失正文和泄漏流。
		raw, err := s.readUpstreamStream(stream)
		if err != nil {
			return stream, fail(stream.StatusCode, "upstream_error", "Basis Points error body could not be read: "+safeError(err))
		}
		// 此时还没有向客户端交付任何字节：密文验不过就同路重发一次。
		if allowEncryptedRetry {
			if retryBody, ok := prepareEncryptedContentRetry(body, stream.StatusCode, raw); ok {
				return s.upstreamStreamAttempt(request, retryBody, c, false)
			}
		}
		return stream, upstreamRequestError(stream.StatusCode, raw, body, c)
	}
	return stream, nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return redactTokenMessage(err.Error())
}

func (s *Service) readUpstreamStream(stream upstreamStream) ([]byte, error) {
	cfg := s.config()
	if stream.StreamID == "" {
		return nil, fail(502, "upstream_transport", "upstream stream ID is empty")
	}
	defer func() { _ = s.call("host.http.stream_close", map[string]any{"stream_id": stream.StreamID}, nil) }()
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	var buffer bytes.Buffer
	for {
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": stream.StreamID}, &chunk); err != nil {
			return nil, fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if chunk.Error != "" {
			return nil, fail(502, "upstream_transport", "Basis Points stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		if len(chunk.Payload) > 0 {
			if buffer.Len()+len(chunk.Payload) > cfg.MaxResponseBytes {
				return nil, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			_, _ = buffer.Write(chunk.Payload)
		}
		if chunk.Done {
			return buffer.Bytes(), nil
		}
	}
}

// consumeUpstreamStream 边读边把上游 SSE 事件交给 handler：
// 不再把整轮响应收进缓冲区，否则长回合里下游长时间零字节 → Cloudflare 524 / 客户端"一直转圈"。
// handler 返回错误即停止读取并关闭上游流。
func (s *Service) consumeUpstreamStream(stream upstreamStream, handler func(event string, data []byte) error) error {
	cfg := s.config()
	if stream.StreamID == "" {
		return fail(502, "upstream_transport", "upstream stream ID is empty")
	}
	defer func() { _ = s.call("host.http.stream_close", map[string]any{"stream_id": stream.StreamID}, nil) }()
	decoder := newSSEDecoder()
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	total := 0
	for {
		if time.Now().After(deadline) {
			return timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": stream.StreamID}, &chunk); err != nil {
			return fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if chunk.Error != "" {
			return fail(502, "upstream_transport", "Basis Points stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		if len(chunk.Payload) > 0 {
			total += len(chunk.Payload)
			if total > cfg.MaxResponseBytes {
				return fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			// 只把解出来的完整事件交出去；单个事件可能跨 chunk。
			if err := decoder.feed(chunk.Payload, func(event, data string) error {
				return handler(event, []byte(data))
			}); err != nil {
				return err
			}
		}
		if chunk.Done {
			return nil
		}
	}
}

type sseDecoder struct {
	buffer strings.Builder
	data   []string
	event  string
}

func newSSEDecoder() *sseDecoder { return &sseDecoder{} }

func (d *sseDecoder) feed(chunk []byte, emit func(event, data string) error) error {
	d.buffer.Write(chunk)
	text := d.buffer.String()
	for {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			d.buffer.Reset()
			d.buffer.WriteString(text)
			return nil
		}
		line := strings.TrimSuffix(text[:index], "\r")
		text = text[index+1:]
		if line == "" {
			if len(d.data) > 0 {
				if err := emit(d.event, strings.Join(d.data, "\n")); err != nil {
					return err
				}
			}
			d.data = nil
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			d.data = append(d.data, strings.TrimPrefix(value, " "))
		}
	}
}

// 仅附加非敏感摘要，不记录对话正文、图片内容或认证信息。
func upstreamRequestError(status int, raw []byte, body map[string]any, c credential) error {
	redacted := string(raw)
	for _, secret := range []string{c.AccessToken, c.AccountID, c.Email} {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	message := redactTokenMessage(errorMessage([]byte(redacted)))
	images, originalDetails := 0, 0
	items, _ := body["input"].([]any)
	for _, value := range items {
		entry := objectValue(value)
		field := inlineImageField(entry)
		if field == "" {
			continue
		}
		parts, _ := entry[field].([]any)
		for _, part := range parts {
			if stringValue(objectValue(part)["type"]) == "input_image" {
				images++
				if stringValue(objectValue(part)["detail"]) == "original" {
					originalDetails++
				}
			}
		}
	}
	tier := "unspecified"
	if value, exists := body["service_tier"]; exists {
		switch stringValue(value) {
		case "auto", "default", "flex", "priority", "scale":
			tier = stringValue(value)
		default:
			tier = "invalid"
		}
	}
	return fail(status, "upstream_error", fmt.Sprintf("Basis Points HTTP %d: %s (reasoning_effort=%s; service_tier=%s; input_images=%d; original_detail_images=%d)", status, message, stringValue(body["reasoning_effort"]), tier, images, originalDetails))
}

// isEncryptedContentRejection 只认"明确说密文验不过"的 400：
// 普通 400、认证、配额与传输错误都不重放请求（边界借 ranxi2001/sub2api v2.8.16）。
func isEncryptedContentRejection(status int, raw []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return false
	}
	errorObject := objectValue(body["error"])
	if errorObject == nil {
		return false
	}
	if code := strings.TrimSpace(stringValue(errorObject["code"])); code != "" {
		return code == "invalid_encrypted_content"
	}
	// 上游偶尔不给 code：只认完整的那句诊断，不把正文里随口提到 encryption 当成这个错误。
	message := strings.ToLower(strings.TrimSpace(stringValue(errorObject["message"])))
	return strings.HasPrefix(message, "the encrypted content ") &&
		strings.Contains(message, "could not be verified") &&
		strings.Contains(message, "could not be decrypted or parsed")
}

// prepareEncryptedContentRetry 从已准备好的请求里只去掉不透明 reasoning，保留消息、工具结果、附件与路由元数据，
// 并要求还剩真实历史可重放。压缩项与密文消息可能是用户上下文的唯一副本，绝不能为了"让请求过"而丢。
func prepareEncryptedContentRetry(body map[string]any, status int, raw []byte) (map[string]any, bool) {
	if !isEncryptedContentRejection(status, raw) {
		return nil, false
	}
	items, ok := body["input"].([]any)
	if !ok {
		return nil, false
	}
	kept := make([]any, 0, len(items))
	removed, hasHistory := false, false
	for _, value := range items {
		item := objectValue(value)
		if item == nil {
			kept = append(kept, value)
			continue
		}
		itemType := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if itemType == "reasoning" && stringValue(item["encrypted_content"]) != "" {
			removed = true
			continue
		}
		if _, exists := item["encrypted_content"]; exists {
			// 除了不透明 reasoning，其它位置的密文都不动：丢弃等于丢用户的上下文。
			return nil, false
		}
		if parts, ok := item["encrypted_function_args"].([]any); ok && len(parts) > 0 {
			return nil, false
		}
		for _, field := range []string{"content", "output"} {
			parts, _ := item[field].([]any)
			for _, part := range parts {
				partObject := objectValue(part)
				if partObject == nil {
					continue
				}
				if stringValue(partObject["type"]) == "encrypted_content" {
					return nil, false
				}
				if _, exists := partObject["encrypted_content"]; exists {
					return nil, false
				}
			}
		}
		// 只有注入的 developer 指令或压缩触发项不算上下文：重放它们生成不出用户的任务。
		role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
		if role == "user" || role == "assistant" || itemType == "agent_message" ||
			itemType == "function_call" || itemType == "function_call_output" {
			hasHistory = true
		}
		kept = append(kept, value)
	}
	if !removed || !hasHistory {
		return nil, false
	}
	retry := cloneObject(body)
	retry["input"] = kept
	return retry, true
}

// 429/503 与上游 5xx（500/502/504）都是"稍后再试"，不是账号故障：BPS 的 requests/minute 是
// 账号级配额，同一账号的其它客户端（例如网页端）也会消耗它。把这类错误直接透传给客户端会
// 诱发"整轮重试"——每轮都要重新上传历史图片，反而把配额烧得更干。这里做有界重试，
// 等待时间设上限以免把请求挂太久。
const (
	rateLimitRetryLimit       = 2
	rateLimitRetryBackoffBase = time.Second
	rateLimitRetryBackoffMax  = 8 * time.Second
)

// rateLimitRetryDelay 决定是否重试以及等待多久。上游给了 Retry-After 就按它等（仅在合理范围内）；
// 要求的等待超过 rateLimitRetryBackoffMax 时放弃重试——此时重试只是拖延，交给客户端自己退避。
func rateLimitRetryDelay(status int, headers http.Header, attempt int) (time.Duration, bool) {
	if attempt >= rateLimitRetryLimit {
		return 0, false
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
	default:
		return 0, false
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil {
			return 0, false
		}
		delay := time.Duration(seconds) * time.Second
		if delay > rateLimitRetryBackoffMax {
			return 0, false
		}
		return delay, true
	}
	delay := rateLimitRetryBackoffBase << uint(attempt)
	if delay > rateLimitRetryBackoffMax {
		delay = rateLimitRetryBackoffMax
	}
	return delay, true
}

// isStaleAttachmentOwnership 认出上游"附件归属校验失败"的 500：说明我们复用的 file_id
// 在上游已经失效（过期或被清理），既不是账号故障，重试同一个请求体也没有意义——
// 清掉附件缓存让下一轮重新上传即可自愈。
func isStaleAttachmentOwnership(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "file ownership")
}
