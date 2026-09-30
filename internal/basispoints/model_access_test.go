package basispoints

// 2026-09-30 线上事故：上游对某个模型回 404（`The model \`…\` does not exist or you do not have access to it.`，
// 其内部还会给模型名贴 `degrade2-luna` / `codex-abuse` 一类标签），CPA 收到后把这套（单凭据插件的唯一）
// 凭据冷却成持续的 503 `auth_unavailable`，且不自愈。这类 404 现在与上游 5xx 一样按内联失败交付。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func modelGoneBody() []byte {
	return jsonBytes(map[string]any{"error": map[string]any{
		"message": "The model `gpt-6-astra-degrade2-luna-1p-codexswic-ev3` does not exist or you do not have access to it.",
	}})
}

// 文案识别：只认"模型不存在/无访问权/模型访问已变更"，其它 404 文案不认。
func TestModelAccessDenialMatchers(t *testing.T) {
	matches := []string{
		"Basis Points HTTP 404: The model `gpt-6-astra-degrade2-luna-1p-codexswic-ev3` does not exist or you do not have access to it. (reasoning_effort=medium; service_tier=unspecified; input_images=0; original_detail_images=0)",
		"Basis Points HTTP 404: Model access has changed. Available models will update automatically. Try again, or contact your workspace admin if no model is available.",
		`{"error":{"code":"model_not_found"}}`,
	}
	for _, message := range matches {
		if !isModelAccessDenial(message) {
			t.Errorf("model-access wording was not recognised: %q", message)
		}
	}
	others := []string{
		"Basis Points HTTP 404: path not found",
		"Basis Points HTTP 500: Unknown error while validating file ownership.",
		"Basis Points HTTP 429: You've exceeded the 1000 request(s) every 1 minute(s) rate limit, please slow down and try again.",
		"Basis Points attachment upload HTTP 429: 429: File upload was rate limited by OpenAI.",
	}
	for _, message := range others {
		if isModelAccessDenial(message) {
			t.Errorf("unrelated wording was wrongly treated as a model-access denial: %q", message)
		}
	}
}

// 分类：404 + 上游错误类别 + 模型访问文案才内联；其它状态码/类别/文案都不动。
func TestUpstreamModelAccessErrorClassification(t *testing.T) {
	access := &APIError{Status: 404, Kind: "upstream_error",
		Message: "Basis Points HTTP 404: The model `x` does not exist or you do not have access to it."}
	if got, ok := asUpstreamModelAccessError(access); !ok || got != access {
		t.Fatalf("model-access 404 was not recognised: %v %v", got, ok)
	}
	upload := &APIError{Status: 404, Kind: "attachment_upload_error",
		Message: "Basis Points attachment upload HTTP 404: Model access has changed."}
	if _, ok := asUpstreamModelAccessError(upload); !ok {
		t.Fatal("attachment upload 404 with model-access wording should be inlined too")
	}

	notInlined := []*APIError{
		{Status: 404, Kind: "upstream_error", Message: "Basis Points HTTP 404: path not found"},
		{Status: 404, Kind: "unsupported_model", Message: "Basis Points HTTP 404: The model `x` does not exist or you do not have access to it."},
		{Status: 400, Kind: "upstream_error", Message: "Basis Points HTTP 400: The model `x` does not exist or you do not have access to it."},
		{Status: 500, Kind: "upstream_error", Message: "Basis Points HTTP 500: The model `x` does not exist or you do not have access to it."},
	}
	for _, candidate := range notInlined {
		if _, ok := asUpstreamModelAccessError(candidate); ok {
			t.Errorf("wrongly inlined: %+v", candidate)
		}
	}
	if _, ok := asUpstreamServerError(notInlined[3]); !ok {
		t.Error("5xx must keep going through asUpstreamServerError")
	}
}

// 流式：404 模型不可用以内联 response.failed 交付（HTTP 200 SSE），且**不重试**。
func TestUpstreamModelAccessErrorStreamsInlineFailure(t *testing.T) {
	service := NewService()
	service.sleep = func(time.Duration) {}
	var mu sync.Mutex
	var emitted []string
	closed := make(chan string, 4)
	streamCalls := 0
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.http.do_stream":
			streamCalls++
			*out.(*upstreamStream) = upstreamStream{StatusCode: 404, StreamID: "upstream-404", Headers: http.Header{"Content-Type": {"application/json"}}}
			return nil
		case "host.http.stream_read":
			*out.(*streamChunk) = streamChunk{Payload: modelGoneBody(), Done: true}
			return nil
		case "host.http.stream_close":
			return nil
		case "host.stream.emit":
			mu.Lock()
			emitted = append(emitted, string(payload.(map[string]any)["payload"].([]byte)))
			mu.Unlock()
			return nil
		case "host.stream.close":
			raw, _ := json.Marshal(payload)
			var request struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &request)
			select {
			case closed <- request.Error:
			default:
			}
			return nil
		}
		return fmt.Errorf("unexpected host method %s", method)
	})
	result, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(namespaceTestSource("function", "get_weather", ""))))
	if err != nil {
		t.Fatalf("model-access 404 was reported as a plugin error (CPA would cool the account): %v", err)
	}
	if result == nil {
		t.Fatal("no stream headers were returned")
	}
	select {
	case closeErr := <-closed:
		if closeErr != "" {
			t.Fatalf("stream closed with an error: %q", closeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream was not closed")
	}
	mu.Lock()
	joined := strings.Join(emitted, "")
	mu.Unlock()
	if !strings.Contains(joined, "response.failed") || !strings.Contains(joined, "upstream_model_unavailable") {
		t.Fatalf("inline failure was not emitted: %q", joined)
	}
	if strings.Contains(joined, "response.completed") {
		t.Fatalf("failure stream claimed completion: %q", joined)
	}
	if streamCalls != 1 {
		t.Fatalf("do_stream calls=%d, want 1 (a 404 must not be retried)", streamCalls)
	}
}

// 非流式（原生 Responses 客户端）：同样内联交付 status=failed + upstream_model_unavailable。
func TestUpstreamModelAccessErrorNonStreamInlineFailure(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.http.do":
			*out.(*upstreamResponse) = upstreamResponse{StatusCode: 404, Headers: http.Header{"Content-Type": {"application/json"}}, Body: modelGoneBody()}
			return nil
		}
		return fmt.Errorf("unexpected host method %s", method)
	})
	request := nonStreamRequest(namespaceTestSource("function", "get_weather", ""))
	request.Format = openAIResponseFormat
	result, err := service.Handle("executor.execute", jsonBytes(request))
	if err != nil {
		t.Fatalf("model-access 404 must be delivered in band, got %v", err)
	}
	payload, _ := result.(map[string]any)
	var body map[string]any
	if err := json.Unmarshal(payload["Payload"].([]byte), &body); err != nil {
		t.Fatal(err)
	}
	if stringValue(body["status"]) != "failed" {
		t.Fatalf("status = %v (body=%v)", body["status"], body)
	}
	if code := stringValue(objectValue(body["error"])["code"]); code != "upstream_model_unavailable" {
		t.Fatalf("failure code = %q", code)
	}
	if !strings.Contains(stringValue(objectValue(body["error"])["message"]), "does not exist or you do not have access") {
		t.Fatalf("upstream wording was not surfaced: %v", body["error"])
	}
}

// 反向用例：与模型访问无关的 404（例如 responses_url 配错）必须照旧抛错，让配置问题保持响亮。
func TestUnexpectedNotFoundStillSurfaces(t *testing.T) {
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.http.do":
			*out.(*upstreamResponse) = upstreamResponse{StatusCode: 404, Headers: http.Header{"Content-Type": {"application/json"}},
				Body: jsonBytes(map[string]any{"error": map[string]any{"message": "path not found"}})}
			return nil
		}
		return fmt.Errorf("unexpected host method %s", method)
	})
	request := nonStreamRequest(namespaceTestSource("function", "get_weather", ""))
	request.Format = openAIResponseFormat
	_, err := service.Handle("executor.execute", jsonBytes(request))
	if err == nil {
		t.Fatal("an unrelated 404 must still surface as an error")
	}
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Status != 404 {
		t.Fatalf("err = %v, want a 404 APIError", err)
	}
}
