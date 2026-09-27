package basispoints

// 第一批搬运的用例：① 模型目录能力同步（上游 v0.1.17）② 入站前置校验（上游 v0.1.16/17）
// ③ 密文验不过时的一次性同路重发（sub2api v2.8.16）。

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ── ① 模型目录：补丁能力同步 + 撤回未实现能力 + 实验工具白名单 ─────────────────

func catalogBody(canonical, alias map[string]any) []byte {
	return jsonBytes(map[string]any{"models": []any{canonical, alias}})
}

func catalogAlias(t *testing.T, svc *Service, body []byte) map[string]json.RawMessage {
	t.Helper()
	result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(body)))
	if err != nil {
		t.Fatalf("catalog intercept failed: %v", err)
	}
	var reply struct{ Body []byte }
	if err := json.Unmarshal(jsonBytes(result), &reply); err != nil {
		t.Fatalf("catalog reply: %v", err)
	}
	if len(reply.Body) == 0 {
		t.Fatal("catalog was not rewritten")
	}
	_, models := decodedCatalog(t, reply.Body)
	if len(models) != 2 {
		t.Fatalf("catalog models = %d, want 2", len(models))
	}
	return models[1]
}

func TestModelCatalogSyncsPatchCapabilityAndWhitelistsExperimentalTools(t *testing.T) {
	svc := NewService()
	alias := catalogAlias(t, svc, catalogBody(
		map[string]any{
			"slug": DefaultUpstreamModel, "context_window": 272000, "max_context_window": 872000,
			"apply_patch_tool_type":        "freeform",
			"experimental_supported_tools": []string{"clock", "send_user_message_async", "code_mode", "review_policy"},
			"multi_agent_version":          "v2",
			"multi_agent_reasoning_effort": "high",
		},
		map[string]any{
			"slug": DefaultModelID, "context_window": 272000, "max_context_window": 272000,
			"apply_patch_tool_type":        "legacy",
			"experimental_supported_tools": []string{"unknown_tool"},
			"multi_agent_version":          "v1",
			"multi_agent_reasoning_effort": "low",
		},
	))
	var patch string
	if err := json.Unmarshal(alias["apply_patch_tool_type"], &patch); err != nil || patch != "freeform" {
		t.Fatalf("apply_patch_tool_type = %q (err=%v), want the canonical value", patch, err)
	}
	for _, field := range []string{"multi_agent_version", "multi_agent_reasoning_effort"} {
		if _, exists := alias[field]; exists {
			t.Fatalf("%s must not be claimed on the alias", field)
		}
	}
	var tools []string
	if err := json.Unmarshal(alias["experimental_supported_tools"], &tools); err != nil {
		t.Fatalf("experimental_supported_tools: %v", err)
	}
	if strings.Join(tools, ",") != "clock,send_user_message_async" {
		t.Fatalf("experimental tools = %v, want the verified whitelist only", tools)
	}
}

func TestModelCatalogDropsStalePatchCapability(t *testing.T) {
	svc := NewService()
	alias := catalogAlias(t, svc, catalogBody(
		map[string]any{"slug": DefaultUpstreamModel, "context_window": 272000, "max_context_window": 872000},
		map[string]any{"slug": DefaultModelID, "context_window": 272000, "max_context_window": 272000,
			"apply_patch_tool_type": "legacy"},
	))
	if _, exists := alias["apply_patch_tool_type"]; exists {
		t.Fatal("a stale patch capability must be removed when the canonical model does not declare it")
	}
}

func TestModelCatalogRejectsInvalidExperimentalTools(t *testing.T) {
	svc := NewService()
	_, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(catalogBody(
		map[string]any{"slug": DefaultUpstreamModel, "context_window": 272000, "max_context_window": 872000,
			"experimental_supported_tools": "clock"},
		map[string]any{"slug": DefaultModelID, "context_window": 272000, "max_context_window": 272000},
	))))
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Kind != "model_metadata_missing" {
		t.Fatalf("err = %v, want model_metadata_missing", err)
	}
}

// ── ② 入站前置校验：结构化 text.format 与代理密文 ──────────────────────────────

func TestValidateTextFormat(t *testing.T) {
	for _, tc := range []struct {
		name string
		text any
		kind string
	}{
		{"absent", nil, ""},
		{"plain-text", map[string]any{"format": map[string]any{"type": "text"}}, ""},
		{"verbosity-only", map[string]any{"verbosity": "low"}, ""},
		{"json-object", map[string]any{"format": map[string]any{"type": "json_object"}}, "unsupported_text_format"},
		{"json-schema", map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}}, "unsupported_text_format"},
		{"unknown-type", map[string]any{"format": map[string]any{"type": "yaml"}}, "invalid_text_format"},
		{"extra-fields", map[string]any{"format": map[string]any{"type": "text", "schema": map[string]any{}}}, "invalid_text_format"},
		{"not-object", "json", "invalid_text_config"},
		{"format-not-object", map[string]any{"format": "json"}, "invalid_text_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTextFormat(tc.text)
			if tc.kind == "" {
				if err != nil {
					t.Fatalf("unexpected rejection: %v", err)
				}
				return
			}
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.Kind != tc.kind {
				t.Fatalf("err = %v, want kind %s", err, tc.kind)
			}
		})
	}
}

func TestValidateAgentMessageEncryption(t *testing.T) {
	encrypted := []any{map[string]any{"type": "agent_message", "content": []any{
		map[string]any{"type": "input_text", "text": "hi"},
		map[string]any{"type": "encrypted_content", "encrypted_content": "cipher"},
	}}}
	var apiError *APIError
	if err := validateAgentMessageEncryption(encrypted); !errors.As(err, &apiError) || apiError.Kind != "unsupported_encrypted_agent_message" {
		t.Fatalf("err = %v, want unsupported_encrypted_agent_message", err)
	}
	plain := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "reasoning-cipher"},
	}
	if err := validateAgentMessageEncryption(plain); err != nil {
		t.Fatalf("reasoning ciphertext must keep its existing handling: %v", err)
	}
}

func TestPrepareResponsesBodyRejectsStructuredFormatAndAgentCiphertext(t *testing.T) {
	svc := NewService()
	base := map[string]any{"model": DefaultModelID,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}}}

	structured := cloneObject(base)
	structured["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}}
	var apiError *APIError
	if _, err := prepareResponsesBody(structured, svc.cfg); !errors.As(err, &apiError) || apiError.Kind != "unsupported_text_format" {
		t.Fatalf("err = %v, want unsupported_text_format", err)
	}

	agent := cloneObject(base)
	agent["input"] = append(agent["input"].([]any), map[string]any{"type": "agent_message", "content": []any{
		map[string]any{"type": "encrypted_content", "encrypted_content": "cipher"}}})
	if _, err := prepareResponsesBody(agent, svc.cfg); !errors.As(err, &apiError) || apiError.Kind != "unsupported_encrypted_agent_message" {
		t.Fatalf("err = %v, want unsupported_encrypted_agent_message", err)
	}

	if _, err := prepareResponsesBody(base, svc.cfg); err != nil {
		t.Fatalf("a plain request must keep working: %v", err)
	}
}

// ── ③ 密文验不过：去掉不透明 reasoning，同路重发一次 ───────────────────────────

func TestIsEncryptedContentRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		raw    string
		want   bool
	}{
		{"code", 400, `{"error":{"code":"invalid_encrypted_content","message":"x"}}`, true},
		{"message", 400, `{"error":{"message":"The encrypted content abc could not be verified: it could not be decrypted or parsed"}}`, true},
		{"other-code", 400, `{"error":{"code":"invalid_request_error","message":"x"}}`, false},
		{"other-message", 400, `{"error":{"message":"encryption is not supported here"}}`, false},
		{"server-error", 500, `{"error":{"code":"invalid_encrypted_content"}}`, false},
		{"not-json", 400, `oops`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEncryptedContentRejection(tc.status, []byte(tc.raw)); got != tc.want {
				t.Fatalf("isEncryptedContentRejection = %t, want %t", got, tc.want)
			}
		})
	}
}

func encryptedRetryBody() map[string]any {
	return map[string]any{"model": DefaultUpstreamModel, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque"},
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "rules"}}},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
	}}
}

func TestPrepareEncryptedContentRetryKeepsHistoryAndDropsOpaqueReasoning(t *testing.T) {
	rejection := []byte(`{"error":{"code":"invalid_encrypted_content"}}`)
	retry, ok := prepareEncryptedContentRetry(encryptedRetryBody(), 400, rejection)
	if !ok {
		t.Fatal("a rejection with real history must be retryable")
	}
	items := retry["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("retry input = %d items, want the reasoning item dropped", len(items))
	}
	for _, value := range items {
		if stringValue(objectValue(value)["type"]) == "reasoning" {
			t.Fatal("the opaque reasoning item survived")
		}
	}
	if stringValue(objectValue(items[0])["role"]) != "user" || stringValue(objectValue(items[2])["type"]) != "function_call_output" {
		t.Fatal("messages or tool results were lost")
	}

	// 其它位置的密文一律不丢。
	broken := encryptedRetryBody()
	broken["input"] = append([]any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "encrypted_content", "encrypted_content": "cipher"}}}}, broken["input"].([]any)...)
	if _, ok := prepareEncryptedContentRetry(broken, 400, rejection); ok {
		t.Fatal("an encrypted message part must block the retry")
	}

	// 只剩注入指令时不能重放。
	noHistory := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque"},
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "rules"}}},
	}}
	if _, ok := prepareEncryptedContentRetry(noHistory, 400, rejection); ok {
		t.Fatal("a body without user history must not be replayed")
	}

	if _, ok := prepareEncryptedContentRetry(encryptedRetryBody(), 500, rejection); ok {
		t.Fatal("only an explicit ciphertext rejection may be replayed")
	}
}

// encRetryHost 第一次 do_stream 返回 400 的密文拒绝，第二次返回正常流。
type encRetryHost struct {
	mu       sync.Mutex
	attempts int
	bodies   []map[string]any
	chunks   [][]byte
	rejected bool
	reject   []byte
	reads    int
	closed   chan struct{}
	once     sync.Once
}

func (h *encRetryHost) call(method string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var request struct {
		StreamID string `json:"stream_id"`
		Body     []byte `json:"body"`
	}
	_ = json.Unmarshal(raw, &request)
	switch method {
	case "host.http.do_stream":
		h.mu.Lock()
		h.attempts++
		attempt := h.attempts
		var body map[string]any
		_ = json.Unmarshal(request.Body, &body)
		h.bodies = append(h.bodies, body)
		h.mu.Unlock()
		status, streamID := 400, "host-rejected"
		if attempt > 1 {
			status, streamID = 200, "host-accepted"
		}
		return json.Unmarshal(jsonBytes(map[string]any{
			"status_code": status, "stream_id": streamID,
			"headers": map[string][]string{"Content-Type": {"text/event-stream"}},
		}), out)
	case "host.http.stream_read":
		h.mu.Lock()
		defer h.mu.Unlock()
		if request.StreamID == "host-rejected" {
			if h.rejected {
				return json.Unmarshal(jsonBytes(map[string]any{"done": true}), out)
			}
			h.rejected = true
			payload := h.reject
			if len(payload) == 0 {
				payload = []byte(`{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified"}}`)
			}
			return json.Unmarshal(jsonBytes(map[string]any{"payload": payload, "done": false}), out)
		}
		if h.reads < len(h.chunks) {
			chunk := h.chunks[h.reads]
			h.reads++
			return json.Unmarshal(jsonBytes(map[string]any{"payload": chunk, "done": false}), out)
		}
		return json.Unmarshal(jsonBytes(map[string]any{"done": true}), out)
	case "host.http.stream_close":
		return nil
	case "host.stream.emit":
		return nil
	case "host.stream.close":
		h.once.Do(func() { close(h.closed) })
		return nil
	}
	return nil
}

func TestUpstreamRetriesEncryptedRejectionOnce(t *testing.T) {
	host := &encRetryHost{closed: make(chan struct{}),
		chunks: [][]byte{
			sseEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_enc", "status": "in_progress"}}),
			sseEvent("response.completed", map[string]any{"response": map[string]any{"id": "resp_enc", "status": "completed",
				"model": DefaultUpstreamModel, "output": []any{messageItem("assistant", "ok")},
				"usage": map[string]any{"total_tokens": json.Number("3")}}}),
		}}
	svc := NewService()
	svc.SetHost(host.call)
	request := ExecutorRequest{HostCallbackID: "cb", StreamID: "stream-1", Format: openAIResponseFormat}
	stream, err := svc.upstreamStream(request, encryptedRetryBody(), credential{AccessToken: "token", AccountID: "account"})
	if err != nil {
		t.Fatalf("the ciphertext rejection was not recovered: %v", err)
	}
	if stream.StatusCode != 200 {
		t.Fatalf("stream status = %d, want the retried 200", stream.StatusCode)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.attempts != 2 {
		t.Fatalf("upstream attempts = %d, want exactly one replay", host.attempts)
	}
	retried := host.bodies[1]["input"].([]any)
	if len(retried) != 3 {
		t.Fatalf("replayed input = %d items, want the opaque reasoning dropped", len(retried))
	}
	var reasoning int
	for _, value := range retried {
		if stringValue(objectValue(value)["type"]) == "reasoning" {
			reasoning++
		}
	}
	if reasoning != 0 {
		t.Fatal("the replay still carries the rejected reasoning item")
	}
}

func TestUpstreamDoesNotRetryOrdinaryRejections(t *testing.T) {
	host := &encRetryHost{closed: make(chan struct{}), reject: []byte(`{"error":{"code":"invalid_request_error","message":"bad request"}}`)}
	svc := NewService()
	svc.SetHost(host.call)
	request := ExecutorRequest{HostCallbackID: "cb", StreamID: "stream-1", Format: openAIResponseFormat}
	_, err := svc.upstreamStream(request, encryptedRetryBody(), credential{AccessToken: "token", AccountID: "account"})
	if err == nil {
		t.Fatal("a plain 400 must still fail")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.attempts != 1 {
		t.Fatalf("upstream attempts = %d, want no replay", host.attempts)
	}
}
