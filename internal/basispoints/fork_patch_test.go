package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖 prod 分支相对上游 v0.1.14 的本地改动：
//   - custom_tool_call 条目必须带 ctc_ 前缀的 id，历史里的错前缀要纠偏；
//   - 畸形中转调用原样放行（不再整条判废），并把类别级诊断回流到下一轮重发提示；
//   - 协议错误内联交付（HTTP 200 + response.failed），不做 5xx 让 CPA 冷却账号；
//   - 原生 Responses 客户端走增量流式桥（文本实时、工具扣留到终态、空闲保活）；
//   - code 参数是字符串的函数工具走"原始代码直传"（code 放源码原文、extended_summary 放其余参数）。
// 信封路由沿用上游 v0.1.14 的 references + code，不再有内层 {tool,args} 包装。
// 上游补齐同一行为后，本文件可连同改动一起删除。

// ── 条目 id 前缀 ─────────────────────────────────────────────────────────────

// 上游按条目类型校验 id 前缀，沿用原生 run_officejs 的 fc_ id 会让后续请求 400：
// Invalid 'input[n].id': 'fc_…'. Expected an ID that begins with 'ctc'.
func TestCustomToolCallUsesCustomItemID(t *testing.T) {
	source := namespaceTestSource("custom", "apply_patch", "")
	native := namespaceTestNative("custom_id", "apply_patch", "*** Begin Patch\n+line\n")
	_, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
	if err != nil || !changed {
		t.Fatalf("transform: changed=%t err=%v", changed, err)
	}
	call := objectValue(response["output"].([]any)[0])
	if call["type"] != "custom_tool_call" {
		t.Fatalf("type = %v", call["type"])
	}
	if id := stringValue(call["id"]); !strings.HasPrefix(id, itemIDPrefixCustom) {
		t.Fatalf("custom item id = %q, want a %s prefix", id, itemIDPrefixCustom)
	}
	if _, present := call["arguments"]; present {
		t.Fatal("custom input was changed into function arguments")
	}
}

// 历史里残留错前缀时要纠偏，否则一条坏历史会让整段对话每次都 400、重试也救不回来。
func TestTranslateInputItemsRepairsItemIDPrefix(t *testing.T) {
	source := map[string]any{} // 目录为空 → 走原样透传分支，正好检验纠偏本身

	poisoned := map[string]any{
		"type": "custom_tool_call", "id": "fc_03ea15adb7ed6a15", "call_id": "call_poisoned",
		"name": "gone_from_catalog", "input": "*** Begin Patch",
	}
	got := objectValue(translateInputItems([]any{poisoned}, clientToolSpecs(source))[0])
	if id := stringValue(got["id"]); id != "ctc_03ea15adb7ed6a15" {
		t.Fatalf("id = %q, want the same body with a ctc_ prefix", id)
	}

	keep := map[string]any{
		"type": "function_call", "id": "fc_keepme", "call_id": "call_keep",
		"name": "gone_from_catalog", "arguments": "{}",
	}
	if again := objectValue(translateInputItems([]any{keep}, clientToolSpecs(source))[0]); stringValue(again["id"]) != "fc_keepme" {
		t.Fatalf("untouched id was rewritten: %#v", again)
	}

	odd := map[string]any{
		"type": "custom_tool_call", "id": "weird_123", "call_id": "call_odd",
		"name": "gone_from_catalog", "input": "x",
	}
	if unknown := objectValue(translateInputItems([]any{odd}, clientToolSpecs(source))[0]); stringValue(unknown["id"]) != "weird_123" {
		t.Fatalf("unknown prefix was rewritten: %#v", unknown)
	}
}

// ── 客户端不认识原样放行的中转调用 ───────────────────────────────────────────

// 客户端拿到原样放行的 run_officejs 调用时会把结果写成 unsupported call。
// 那不是执行结果，要换成重发提示，让模型下一轮把中转载荷写对。
func TestTranslateInputItemsRewritesUnsupportedTransportResult(t *testing.T) {
	source := namespaceTestSource("function", "exec_command", "")
	for _, output := range []any{"unsupported call: run_officejs", "Unsupported tool: functions.run_officejs"} {
		got := objectValue(translateInputItems([]any{map[string]any{
			"type": "function_call_output", "call_id": "call_unknown", "output": output,
		}}, clientToolSpecs(source))[0])
		if got["output"] != transportRetryHint {
			t.Fatalf("output for %q = %v, want the retry hint", output, got["output"])
		}
		if got["type"] != "function_call_output" {
			t.Fatalf("type = %v", got["type"])
		}
	}

	normal := map[string]any{"type": "function_call_output", "call_id": "call_ok", "output": "Exit code: 0"}
	if got := objectValue(translateInputItems([]any{normal}, clientToolSpecs(source))[0]); got["output"] != "Exit code: 0" {
		t.Fatalf("normal tool output was rewritten: %#v", got)
	}
}

// ── references 路由（上游 v0.1.14 形状）─────────────────────────────────────

// custom 工具的正文按原文直传，不套 JSON：引号、反斜杠、多行内容一字不差。
func TestReferencesRoutingKeepsCustomTextRaw(t *testing.T) {
	source := namespaceTestSource("custom", "apply_patch", "")
	raw := "*** Begin Patch\n*** Update File: a.js\n-.agent-task-card[data-task-id=\"1\"]\n+const re = /\\d+\\s*\"quoted\"/\n*** End Patch\n"
	native := namespaceTestNative("raw_custom", "apply_patch", raw)
	call, err := extractNativeClientToolCall(native, clientToolSpecs(source))
	if err != nil {
		t.Fatalf("raw custom relay was not decoded: %v", err)
	}
	if call["type"] != "custom_tool_call" || call["input"] != raw {
		t.Fatalf("raw input was altered: %#v", call)
	}
	// 回放必须带原始原生条目，否则下一轮历史与上游身份不一致。
	_, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
	if err != nil || !changed {
		t.Fatalf("transform: changed=%t err=%v", changed, err)
	}
	replay := translateInputItems([]any{objectValue(response["output"].([]any)[0])}, clientToolSpecs(source))
	if len(replay) != 1 || !reflect.DeepEqual(objectValue(replay[0]), native) {
		t.Fatalf("replay = %#v, want the original native item", replay[0])
	}
}

// 目录里没有原生条目时，custom 回放必须无损：原文里的引号、反斜杠、换行都要原样解回来。
func TestCustomToolHistoryReplayPreservesRawText(t *testing.T) {
	source := namespaceTestSource("custom", "apply_patch", "")
	raw := "*** Begin Patch\n+quotes \" and \\path\\ and /regex\\d+/\n*** End Patch\n"
	call := map[string]any{
		"type": "custom_tool_call", "id": "ctc_missing", "call_id": "call_missing",
		"name": "apply_patch", "input": raw,
	}
	replayed := objectValue(translateInputItems([]any{call}, clientToolSpecs(source))[0])
	if replayed["name"] != transportName {
		t.Fatalf("replay type = %#v", replayed)
	}
	envelope, err := transportEnvelope(replayed)
	if err != nil {
		t.Fatalf("replay envelope: %v", err)
	}
	if stringValue(envelope["tool"]) != "apply_patch" {
		t.Fatalf("replay envelope = %#v", envelope)
	}
	if envelope["args"] != any(raw) {
		t.Fatalf("replay lost the raw text: %#v", envelope["args"])
	}
}

// ── 协议错误内联交付 ─────────────────────────────────────────────────────────

// 协议错误必须"内联交付"：不能再是 5xx，否则 CPA 会冷却账号并形成 503 墙。
func TestProtocolErrorDeliversInlineFailure(t *testing.T) {
	source := namespaceTestSource("function", "js", "mcp__node_repl")
	native := namespaceTestNative("native", "mcp__node_repl.js", map[string]any{})
	native["name"] = "read_sheets_metadata" // 服务器注入的原生工具：不是我们的运输调用
	_, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
	protocolErr, ok := asProtocolError(err)
	if !ok {
		t.Fatalf("err = %v, want a protocol error", err)
	}
	if protocolErr.Kind != "invalid_tool_call" {
		t.Fatalf("kind = %q", protocolErr.Kind)
	}
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.Status >= 500 {
		t.Fatalf("protocol error still carries HTTP %d", apiError.Status)
	}

	stream := string(syntheticFailureStream("resp_test", protocolErr.Kind, protocolErr.Message))
	if !strings.Contains(stream, "event: response.failed") || !strings.Contains(stream, `"status":"failed"`) {
		t.Fatalf("failure stream = %s", stream)
	}
	if !strings.Contains(stream, protocolErr.Kind) || !strings.Contains(stream, streamDoneMarker) {
		t.Fatalf("failure stream is missing its code or terminator: %s", stream)
	}
	if strings.Contains(stream, "response.completed") {
		t.Fatalf("failure stream claimed completion: %s", stream)
	}

	var body map[string]any
	if err := json.Unmarshal(failureResponseBody("", protocolErr.Kind, protocolErr.Message), &body); err != nil {
		t.Fatal(err)
	}
	if stringValue(body["status"]) != "failed" || stringValue(objectValue(body["error"])["code"]) != protocolErr.Kind {
		t.Fatalf("failure body = %v", body)
	}
}

// 非流式请求遇到本地协议问题也必须内联交付（HTTP 200 语义），不能变成 5xx 让 CPA 冷却账号。
func TestNonStreamProtocolErrorDeliversInlineFailure(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	injected := namespaceTestNative("injected", "get_weather", map[string]any{"city": "Tokyo"})
	injected["name"] = "read_sheets_metadata"
	host, state := newRecordingHost(t)
	state.plainBody = jsonBytes(map[string]any{"id": "resp_inline", "status": "completed", "output": []any{injected}})
	state.plainHeaders = http.Header{"Content-Type": {"application/json"}}
	service := NewService()
	service.SetHost(host)
	request := nonStreamRequest(source)
	request.Format = openAIResponseFormat
	result, err := service.Handle("executor.execute", jsonBytes(request))
	if err != nil {
		t.Fatalf("protocol failure must be delivered in band, got %v", err)
	}
	payload, _ := result.(map[string]any)
	var body map[string]any
	if err := json.Unmarshal(payload["Payload"].([]byte), &body); err != nil {
		t.Fatal(err)
	}
	if stringValue(body["status"]) != "failed" {
		t.Fatalf("status = %v", body["status"])
	}
	if code := stringValue(objectValue(body["error"])["code"]); code != "invalid_tool_call" {
		t.Fatalf("failure code = %q", code)
	}
	headers, _ := payload["Headers"].(http.Header)
	if headers.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", headers.Get("Content-Type"))
	}
	if strings.Contains(string(payload["Payload"].([]byte)), "read_sheets_metadata") {
		t.Fatal("the in-band failure leaked the injected native tool")
	}
}

// ── 畸形中转：原样放行 + 诊断回流 ────────────────────────────────────────────

func brokenRelayFixture(key, code string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_broken", "call_id": "call_broken", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"references": []any{key}, "code": code,
		})),
	}
}

// 畸形中转调用要给出"类别 + 偏移"级诊断：既进内联失败消息，也附到下一轮重发提示。
func TestMalformedRelayCarriesDiagnostic(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	specs := clientToolSpecs(source)
	broken := brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	if _, _, _, err := transformResponseBodyPassThrough(jsonBytes(map[string]any{"output": []any{broken}}), source); err != nil {
		t.Fatalf("a malformed transport item must be passed through, not fail the turn: %v", err)
	}
	next := objectValue(translateInputItems([]any{map[string]any{
		"type": "function_call_output", "call_id": "call_broken", "output": "unsupported call: run_officejs",
	}}, specs)[0])
	hint := stringValue(next["output"])
	if !strings.Contains(hint, "Diagnostic: code invalid_json byte_offset=") {
		t.Fatalf("retry hint = %q, want the parse diagnostic", hint)
	}

	injected := namespaceTestNative("injected", "get_weather", map[string]any{"city": "Tokyo"})
	injected["name"] = "read_sheets_metadata"
	_, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{injected}}), source)
	protocolErr, ok := asProtocolError(err)
	if !ok || !strings.Contains(protocolErr.Message, "Diagnostic: outer_not_transport") {
		t.Fatalf("protocol error = %v", err)
	}
}

// ── 增量流式 ─────────────────────────────────────────────────────────────────

func sseEvent(kind string, body map[string]any) []byte {
	body["type"] = kind
	return []byte("event: " + kind + "\ndata: " + string(jsonBytes(body)) + "\n\n")
}

type emittedEvent struct {
	kind    string
	payload map[string]any
}

func hasKind(events []emittedEvent, kind string) bool {
	for _, event := range events {
		if event.kind == kind {
			return true
		}
	}
	return false
}

func indicesOf(events []emittedEvent, kind string) []int {
	var found []int
	for index, event := range events {
		if event.kind == kind {
			found = append(found, index)
		}
	}
	return found
}

// parseEmitted 把桥接器写出的字节流拆成事件列表（保活注释单独标出来）。
func parseEmitted(t *testing.T, state *recordingHostState) []emittedEvent {
	t.Helper()
	var events []emittedEvent
	for _, block := range strings.Split(strings.Join(state.snapshot(), ""), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if strings.HasPrefix(block, ":") {
			events = append(events, emittedEvent{kind: ": keepalive"})
			continue
		}
		var kind, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			case strings.HasPrefix(line, "data: "):
				data += strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
		}
		if data == "[DONE]" {
			events = append(events, emittedEvent{kind: "[DONE]"})
			continue
		}
		var payload map[string]any
		if data != "" {
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("emitted block is not valid JSON: %q (%v)", block, err)
			}
		}
		events = append(events, emittedEvent{kind: kind, payload: payload})
	}
	return events
}

// 文本增量必须在终态之前下发；工具调用要扣留到 response.completed 之后才合成发出，
// 且绝不能把半截的原生参数交给客户端。
func TestStreamForwardsTextBeforeCompletionAndWithholdsTools(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	native := namespaceTestNative("stream_tool", "get_weather", map[string]any{"city": "Tokyo"})
	toolID := stringValue(native["id"])
	chunks := [][]byte{
		sseEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_s", "status": "in_progress"}}),
		sseEvent("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_1", "delta": "hello "}),
		sseEvent("response.output_item.added", map[string]any{"output_index": 1, "item": native}),
		sseEvent("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": toolID, "delta": `{"city":`}),
		sseEvent("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_1", "delta": "world"}),
		sseEvent("response.output_item.done", map[string]any{"output_index": 1, "item": native}),
		sseEvent("response.completed", map[string]any{"response": map[string]any{
			"id": "resp_s", "status": "completed",
			"output": []any{
				map[string]any{"type": "message", "role": "assistant", "id": "msg_1",
					"content": []any{map[string]any{"type": "output_text", "text": "hello world"}}},
				native,
			},
		}}),
	}
	host, state := newRecordingHost(t, chunks...)
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	waitForStream(t, state)
	if state.closeError != "" {
		t.Fatalf("stream closed with an error: %q", state.closeError)
	}
	events := parseEmitted(t, state)

	joined := strings.Join(state.snapshot(), "")
	if !strings.Contains(joined, `"delta":"hello "`) {
		t.Fatal("text delta was never forwarded")
	}
	toolEvents := indicesOf(events, "response.function_call_arguments.delta")
	completed := indicesOf(events, "response.completed")
	if len(toolEvents) != 1 || len(completed) != 1 {
		t.Fatalf("tool deltas=%v completed=%v", toolEvents, completed)
	}
	if toolEvents[0] > completed[0] {
		t.Fatalf("tool events must be synthesized before the terminal event: %v vs %v", toolEvents, completed)
	}
	// 客户端看到的是目录工具的完整参数，而不是上游半截的原生参数。
	delta := stringValue(objectValue(events[toolEvents[0]].payload)["delta"])
	if delta != `{"city":"Tokyo"}` {
		t.Fatalf("client arguments = %q", delta)
	}
	if events[len(events)-1].kind != "[DONE]" {
		t.Fatalf("stream does not end with the done marker: %q", events[len(events)-1].kind)
	}
	if kind := events[0].kind; kind != "response.created" {
		t.Fatalf("first event = %q", kind)
	}
	completedPayload := objectValue(events[completed[0]].payload)
	output, _ := objectValue(completedPayload["response"])["output"].([]any)
	if len(output) != 2 || stringValue(objectValue(output[1])["name"]) != "get_weather" {
		t.Fatalf("completed output = %#v", output)
	}
	// 被扣留的原生 added/done 没有泄漏：下半段不该出现中转工具名。
	if strings.Contains(joined, transportName) {
		t.Fatal("the withheld native transport item leaked downstream")
	}
}

// 上游长时间不出事件时，必须持续向下游写 SSE 注释保活（否则 Cloudflare 会 524）。
// 直接测桥接器实例，间隔按实例注入，避免动包级变量产生竞争。
func TestStreamKeepaliveCoversUpstreamSilence(t *testing.T) {
	if newToolStreamBridge(func([]byte) error { return nil }, map[string]any{}).keepalive != streamKeepaliveInterval {
		t.Fatal("a new bridge must inherit the configured keepalive interval")
	}
	var mu sync.Mutex
	var emitted []string
	bridge := newToolStreamBridge(func(payload []byte) error {
		mu.Lock()
		emitted = append(emitted, string(payload))
		mu.Unlock()
		return nil
	}, map[string]any{})
	bridge.keepalive = 20 * time.Millisecond
	stop := bridge.startKeepalive()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		joined := strings.Join(emitted, "")
		mu.Unlock()
		if strings.Contains(joined, streamKeepaliveComment) {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatal("no keepalive was emitted while the stream was silent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	mu.Lock()
	before := len(emitted)
	mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	after := len(emitted)
	mu.Unlock()
	if after != before {
		t.Fatalf("keepalive kept writing after stop: %d -> %d", before, after)
	}
}

// 上游以 response.failed 收尾：原样转达给客户端、干净关闭，不能变成插件 5xx。
func TestStreamUpstreamFailureIsForwarded(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	chunks := [][]byte{
		sseEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_f", "status": "in_progress"}}),
		sseEvent("response.failed", map[string]any{"response": map[string]any{"id": "resp_f", "status": "failed",
			"error":  map[string]any{"code": "server_error", "message": "upstream exploded"},
			"output": []any{namespaceTestNative("half", "get_weather", map[string]any{"city": "Oslo"})}}}),
	}
	host, state := newRecordingHost(t, chunks...)
	service := NewService()
	service.SetHost(host)
	result, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source)))
	if err != nil {
		t.Fatalf("upstream failure was reported as a plugin error: %v", err)
	}
	if result == nil {
		t.Fatal("no stream headers were returned")
	}
	waitForStream(t, state)
	if state.closeError != "" {
		t.Fatalf("stream closed with an error: %q", state.closeError)
	}
	events := parseEmitted(t, state)
	if !hasKind(events, "response.failed") {
		t.Fatal("upstream response.failed was not forwarded")
	}
	if hasKind(events, "response.completed") {
		t.Fatal("a failed turn must not claim completion")
	}
	joined := strings.Join(state.snapshot(), "")
	if !strings.Contains(joined, "upstream exploded") {
		t.Fatal("upstream failure reason was dropped")
	}
	if !hasKind(events, "[DONE]") {
		t.Fatal("failed stream did not terminate cleanly")
	}
	if strings.Contains(joined, "Oslo") {
		t.Fatal("an unfinished native tool item leaked downstream")
	}
}

// 终态里出现客户端目录之外的工具：整轮以内联 response.failed 交付（不是 5xx）。
func TestStreamCompletedWithNonCatalogToolStaysInBand(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	injected := namespaceTestNative("injected", "get_weather", map[string]any{"city": "Tokyo"})
	injected["name"] = "read_sheets_metadata"
	chunks := [][]byte{
		sseEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_n", "status": "in_progress"}}),
		sseEvent("response.output_item.done", map[string]any{"output_index": 0, "item": injected}),
		sseEvent("response.completed", map[string]any{"response": map[string]any{
			"id": "resp_n", "status": "completed", "output": []any{injected}}}),
	}
	host, state := newRecordingHost(t, chunks...)
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatalf("protocol failure was reported as a plugin error: %v", err)
	}
	waitForStream(t, state)
	if state.closeError != "" {
		t.Fatalf("stream closed with an error: %q", state.closeError)
	}
	events := parseEmitted(t, state)
	if hasKind(events, "response.completed") {
		t.Fatal("a protocol error must not be reported as a completed response")
	}
	failed := indicesOf(events, "response.failed")
	if len(failed) != 1 {
		t.Fatalf("expected exactly one response.failed, got %v", failed)
	}
	code := stringValue(objectValue(objectValue(objectValue(events[failed[0]].payload)["response"])["error"])["code"])
	if code != "invalid_tool_call" {
		t.Fatalf("failure code = %q", code)
	}
}

// 已经下发过真实内容之后上游断流（传输故障）：按内联失败收尾，不再让 CPA 记到账号上。
func TestStreamTransportFailureAfterOutputStaysInBand(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	chunks := [][]byte{
		sseEvent("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_1", "delta": "partial"}),
	}
	host, state := newRecordingHost(t, chunks...)
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatalf("mid-stream failure must be delivered in band, got %v", err)
	}
	waitForStream(t, state)
	if state.closeError != "" {
		t.Fatalf("stream closed with a plugin error: %q", state.closeError)
	}
	events := parseEmitted(t, state)
	if !hasKind(events, "response.failed") {
		t.Fatal("a truncated stream must end with an inline response.failed")
	}
	if hasKind(events, "response.completed") {
		t.Fatal("a truncated stream must not claim completion")
	}
}

// 终态已下发之后再遇到读取错误（上游断流/超时）：只能收尾，不能补第二个终态事件。
func TestStreamCompletedThenTransportErrorKeepsSingleTerminal(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	chunks := [][]byte{
		sseEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_t", "status": "in_progress"}}),
		sseEvent("response.completed", map[string]any{"response": map[string]any{
			"id": "resp_t", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "id": "msg_1",
				"content": []any{map[string]any{"type": "output_text", "text": "done"}}}},
		}}),
	}
	host, state := newRecordingHost(t, chunks...)
	state.readError = "upstream died after the terminal event"
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatal(err)
	}
	waitForStream(t, state)
	if state.closeError != "" {
		t.Fatalf("a cleanly completed turn must not be closed with an error: %q", state.closeError)
	}
	events := parseEmitted(t, state)
	if completed := indicesOf(events, "response.completed"); len(completed) != 1 {
		t.Fatalf("terminal event count = %v", completed)
	}
	if hasKind(events, "response.failed") {
		t.Fatal("a second terminal (response.failed) was emitted after completion")
	}
}

// 保活注释不算"已交付内容"：上游一声不响、只发出保活后断流，必须按传输故障收尾，
// 让 CPA 可以把账号判为故障并换凭据重试；同时 stop 之后不能再有保活写入。
func TestStreamKeepaliveOnlySilenceThenTransportErrorIsHardFailure(t *testing.T) {
	previous := streamKeepaliveInterval
	streamKeepaliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { streamKeepaliveInterval = previous })

	source := namespaceTestSource("function", "get_weather", "")
	host, state := newRecordingHost(t)
	state.gate = make(chan struct{})
	state.readError = "upstream never answered"
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(state.snapshot(), ""), streamKeepaliveComment) {
		if time.Now().After(deadline) {
			t.Fatal("no keepalive was emitted while the upstream stayed silent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(state.gate)
	waitForStream(t, state)
	if state.closeError == "" {
		t.Fatal("a transport failure that delivered no content must stay a hard error")
	}
	events := parseEmitted(t, state)
	if hasKind(events, "response.failed") || hasKind(events, "response.completed") {
		t.Fatal("a content-free transport failure was in-banded as a terminal event")
	}
	before := len(state.snapshot())
	time.Sleep(80 * time.Millisecond)
	if after := len(state.snapshot()); after != before {
		t.Fatalf("keepalive kept writing after the stream closed: %d -> %d", before, after)
	}
}

// 上游用明文 error 事件收尾时不能吞掉：要把真实原因转达给客户端，
// 否则只会得到"流没有正常收尾"这种无从下手的诊断。
func TestStreamPlaintextErrorEventIsSurfaced(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	chunks := [][]byte{[]byte("event: error\ndata: upstream exploded in plain text\n\n")}
	host, state := newRecordingHost(t, chunks...)
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatal(err)
	}
	waitForStream(t, state)
	events := parseEmitted(t, state)
	failed := indicesOf(events, "response.failed")
	if len(failed) != 1 {
		t.Fatalf("expected exactly one response.failed, got %v", failed)
	}
	inner := objectValue(objectValue(objectValue(events[failed[0]].payload)["response"])["error"])
	if code := stringValue(inner["code"]); code != "basispoints_stream_error" {
		t.Fatalf("failure code = %q", code)
	}
	if message := stringValue(inner["message"]); !strings.Contains(message, "upstream exploded in plain text") {
		t.Fatalf("failure message = %q", message)
	}
}

// 分流守卫：非原生 Responses 格式（codex 等）必须继续走上游的全量缓冲路径，
// 由 executorStreamPayloads 拆成逐条 data 帧。
func TestNonResponsesFormatKeepsBufferedStreaming(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	native := namespaceTestNative("buffered", "get_weather", map[string]any{"city": "Tokyo"})
	completed := sseEvent("response.completed", map[string]any{"response": map[string]any{
		"id": "resp_b", "status": "completed", "output": []any{native},
	}})
	host, state := newRecordingHost(t, completed)
	service := NewService()
	service.SetHost(host)
	request := streamRequest(source)
	request.Format = "codex"
	if _, err := service.Handle("executor.execute_stream", jsonBytes(request)); err != nil {
		t.Fatal(err)
	}
	waitForStream(t, state)
	emitted := state.snapshot()
	if len(emitted) < 5 {
		t.Fatalf("buffered codex path must emit one frame per event, got %d: %v", len(emitted), emitted)
	}
	for _, frame := range emitted {
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("codex frames must be single data lines: %q", frame)
		}
	}
	if !strings.Contains(strings.Join(emitted, ""), `"type":"response.completed"`) {
		t.Fatalf("buffered stream lost its terminal event: %v", emitted)
	}
}

// ── 分流策略：放行只针对"载荷写坏" ───────────────────────────────────────────

// 分流策略：畸形中转载荷在严格路径上按协议错误返回（服务层用上游的"最多重生成一次"
// 把它救回来），只有增量流式桥的放行路径才把它原样交给客户端。
// 目录/tool_choice 这类策略冲突不属于"载荷写坏"，两条路径都必须报错。
func TestMalformedRelayPassthroughIsPolicyScoped(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	broken := brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	body := jsonBytes(map[string]any{"output": []any{broken}})
	_, _, _, err := transformResponseBody(body, source)
	protocolErr, ok := asProtocolError(err)
	if !ok || protocolErr.Kind != "invalid_tool_call" {
		t.Fatalf("the strict path must reject a malformed relay so the service can regenerate once: %v", err)
	}
	_, response, changed, err := transformResponseBodyPassThrough(body, source)
	if err != nil || !changed {
		t.Fatalf("pass-through path: changed=%t err=%v", changed, err)
	}
	if passed := objectValue(response["output"].([]any)[0]); !reflect.DeepEqual(passed, broken) {
		t.Fatalf("passed-through item changed: %#v", passed)
	}

	unknown := brokenRelayFixture("not_in_catalog", `{}`)
	unknownBody := jsonBytes(map[string]any{"output": []any{unknown}})
	transforms := map[string]func([]byte, map[string]any) ([]byte, map[string]any, bool, error){
		"strict":      transformResponseBody,
		"passthrough": transformResponseBodyPassThrough,
	}
	for name, transform := range transforms {
		if _, _, _, err := transform(unknownBody, source); err == nil {
			t.Fatalf("%s path accepted a tool outside the catalog", name)
		}
	}
}

// references 指向目录外的工具：属于策略冲突，两条路径都必须报错，不能放行。
func TestUnregisteredReferenceIsNotPassedThrough(t *testing.T) {
	source := namespaceTestSource("custom", "apply_patch", "")
	native := brokenRelayFixture("unregistered_tool", "*** Begin Patch\n+line\n")
	diagnostic := relayDiagnostic(native, clientToolSpecs(source))
	if !strings.HasPrefix(diagnostic, "tool_not_in_catalog:") {
		t.Fatalf("diagnostic = %q, want a catalog conflict", diagnostic)
	}
	if isRelayShapeDefect(diagnostic) {
		t.Fatal("a catalog conflict was classified as a malformed payload")
	}
	body := jsonBytes(map[string]any{"output": []any{native}})
	transforms := map[string]func([]byte, map[string]any) ([]byte, map[string]any, bool, error){
		"strict":      transformResponseBody,
		"passthrough": transformResponseBodyPassThrough,
	}
	for name, transform := range transforms {
		if _, _, _, err := transform(body, source); err == nil {
			t.Fatalf("%s path passed through a tool outside the catalog", name)
		}
	}
}

// 放行的畸形条目同样参与 call_id 唯一性校验，否则下一轮历史里两个同名工具结果无从配对。
func TestDuplicateCallIDAmongMalformedRelaysFails(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	broken := func() map[string]any {
		return brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	}
	body := jsonBytes(map[string]any{"output": []any{broken(), broken()}})
	_, _, _, err := transformResponseBodyPassThrough(body, source)
	protocolErr, ok := asProtocolError(err)
	if !ok || !strings.Contains(protocolErr.Message, "duplicate_call_id") {
		t.Fatalf("err = %v, want a duplicate_call_id protocol error", err)
	}
}

// ── 原始代码直传（code 参数是字符串的函数工具）────────────────────────────────

func functionCodeSource() map[string]any {
	return map[string]any{"tools": []any{map[string]any{
		"type": "function", "name": "mcp__cua_repl.js",
		"parameters": map[string]any{
			"type": "object", "required": []any{"code"},
			"properties": map[string]any{
				"code":       map[string]any{"type": "string"},
				"timeout_ms": map[string]any{"type": "integer"},
				"title":      map[string]any{"type": "string"},
			},
		},
	}}}
}

// 形态：summary 是 codex2api.function_code/<工具名> 标记，references 指向同一个目录键，
// code 是源码原文，extended_summary 是其余参数的 JSON 对象。
func functionCodeNative(key, code string, metadata any) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_function_code", "call_id": "call_function_code", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          functionCodeMarker(key),
			"references":       []any{key},
			"extended_summary": metadata,
			"code":             code,
		})),
	}
}

// 源码原文必须一字不差地成为目录工具的 code 参数，其余参数从 extended_summary 合并。
func TestFunctionCodeTransportKeepsSourceVerbatim(t *testing.T) {
	source := functionCodeSource()
	code := "const re = /\\d+\\s*\"quoted\"/;\nconsole.log(`tab\there`);\n"
	native := functionCodeNative("mcp__cua_repl.js", code, string(jsonBytes(map[string]any{"timeout_ms": 1500, "title": "probe"})))
	call, err := extractNativeClientToolCall(native, clientToolSpecs(source))
	if err != nil {
		t.Fatalf("function code transport was not decoded: %v", err)
	}
	if call["type"] != "function_call" || call["name"] != "mcp__cua_repl.js" {
		t.Fatalf("client identity = %#v", call)
	}
	if id := stringValue(call["id"]); !strings.HasPrefix(id, itemIDPrefixFunction) {
		t.Fatalf("function item id = %q", id)
	}
	args := parseArguments(call["arguments"])
	if args == nil {
		t.Fatal("merged arguments are not a JSON object")
	}
	if args["code"] != code || args["title"] != "probe" || args["timeout_ms"] != json.Number("1500") {
		t.Fatalf("merged arguments = %#v", args)
	}
	_, _, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
	if err != nil || !changed {
		t.Fatalf("transform: changed=%t err=%v", changed, err)
	}
	// 普通 JSON 形状也要继续可用：code 直接放整个参数对象。
	plain := map[string]any{
		"type": "function_call", "id": "fc_plain", "call_id": "call_plain", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"references": []any{"mcp__cua_repl.js"},
			"code":       string(jsonBytes(map[string]any{"code": "js()", "timeout_ms": 10})),
		})),
	}
	plainCall, err := extractNativeClientToolCall(plain, clientToolSpecs(source))
	if err != nil {
		t.Fatalf("plain JSON function shape must stay supported: %v", err)
	}
	if args := parseArguments(plainCall["arguments"]); args["code"] != "js()" || args["timeout_ms"] != json.Number("10") {
		t.Fatalf("plain shape arguments = %#v", args)
	}
}

// 原始代码直传写坏时不能伪造调用；策略冲突两条路径都必须报错，只有"载荷写坏"能在放行路径通过。
func TestFunctionCodeTransportMismatchIsRejected(t *testing.T) {
	source := functionCodeSource()
	customSource := namespaceTestSource("custom", "apply_patch", "")
	noCodeParameter := map[string]any{"tools": []any{map[string]any{
		"type": "function", "name": "run_query",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"sql": map[string]any{"type": "string"}}},
	}}}
	for _, tc := range []struct {
		name      string
		source    map[string]any
		key       string
		code      any
		extended  any
		marker    string
		wantShape bool
	}{
		{"custom-tool", customSource, "apply_patch", "*** Begin Patch", `{}`, "", false},
		{"no-code-parameter", noCodeParameter, "run_query", "SELECT 1", `{}`, "", false},
		{"unknown-tool", source, "not_in_catalog", "x", `{}`, "", false},
		{"marker-names-another-tool", source, "mcp__cua_repl.js", "x", `{}`, "codex2api.function_code/not_in_catalog", false},
		{"metadata-not-json", source, "mcp__cua_repl.js", "x", "not json", "", true},
		{"metadata-duplicates-code", source, "mcp__cua_repl.js", "x", `{"code":"y"}`, "", false},
		{"code-not-string", source, "mcp__cua_repl.js", map[string]any{"a": 1}, `{}`, "", false},
		{"schema-mismatch", source, "mcp__cua_repl.js", "x", `{"timeout_ms":"soon"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := tc.marker
			if marker == "" {
				marker = functionCodeMarker(tc.key)
			}
			native := functionCodeNative(tc.key, "", tc.extended)
			native["arguments"] = string(jsonBytes(map[string]any{
				"summary": marker, "references": []any{tc.key}, "code": tc.code, "extended_summary": tc.extended,
			}))
			specs := clientToolSpecs(tc.source)
			call, err := extractNativeClientToolCall(native, specs)
			if call != nil || err == nil {
				t.Fatalf("malformed function code envelope produced a call: %#v (err=%v)", call, err)
			}
			diagnostic := relayDiagnostic(native, specs)
			if got := isRelayShapeDefect(diagnostic); got != tc.wantShape {
				t.Fatalf("diagnostic %q shape-classified=%t, want %t", diagnostic, got, tc.wantShape)
			}
			body := jsonBytes(map[string]any{"output": []any{native}})
			transforms := map[string]func([]byte, map[string]any) ([]byte, map[string]any, bool, error){
				"strict": transformResponseBody, "passthrough": transformResponseBodyPassThrough,
			}
			for name, transform := range transforms {
				_, _, changed, err := transform(body, tc.source)
				if tc.wantShape && name == "passthrough" {
					if err != nil || !changed {
						t.Fatalf("a malformed payload must be passed through: changed=%t err=%v", changed, err)
					}
					continue
				}
				if err == nil {
					t.Fatalf("%s path accepted %s", name, tc.name)
				}
			}
		})
	}
}

// 历史回放：这类工具的调用要重新编码成原始代码直传形态，源码一字不差，并能原样解回来。
func TestFunctionCodeTransportHistoryReplay(t *testing.T) {
	source := functionCodeSource()
	code := "await page.click(\"button[data-id=\\\"1\\\"]\");\n"
	call := map[string]any{
		"type": "function_call", "id": "fc_replay", "call_id": "call_replay", "name": "mcp__cua_repl.js",
		"arguments": string(jsonBytes(map[string]any{"code": code, "timeout_ms": json.Number("1500")})),
		"status":    "completed",
	}
	replayed := objectValue(translateInputItems([]any{call}, clientToolSpecs(source))[0])
	if replayed["name"] != transportName {
		t.Fatalf("replay did not use the transport: %#v", replayed)
	}
	outer := parseArguments(replayed["arguments"])
	if !reflect.DeepEqual(outer["references"], []any{"mcp__cua_repl.js"}) || outer["code"] != code {
		t.Fatalf("replay envelope = %#v", outer)
	}
	extra, reason := parseRelayObject(outer["extended_summary"])
	if reason != "" || extra["timeout_ms"] != json.Number("1500") {
		t.Fatalf("replay arguments = %#v (reason=%q)", extra, reason)
	}
	if _, duplicated := extra["code"]; duplicated {
		t.Fatal("replay duplicated code inside the arguments object")
	}
	roundTrip, err := extractNativeClientToolCall(replayed, clientToolSpecs(source))
	if err != nil {
		t.Fatalf("replayed envelope was not decodable: %v", err)
	}
	if !reflect.DeepEqual(parseArguments(roundTrip["arguments"]), parseArguments(call["arguments"])) {
		t.Fatalf("round trip changed the arguments: %#v", roundTrip)
	}
}

// 目录与提醒必须教会模型这个形态，且只对"code 是字符串"的函数工具生效。
func TestFunctionCodeTransportIsDocumentedOnlyForCodeTools(t *testing.T) {
	source := functionCodeSource()
	catalog := clientToolProtocolInstructions(source)
	for _, want := range []string{
		"mcp__cua_repl.js",
		"put that source directly in code and put its other arguments as one JSON object in extended_summary",
	} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("catalog is missing %q", want)
		}
	}
	if reminder := clientToolProtocolReminder(source); !strings.Contains(reminder, "mcp__cua_repl.js takes source text") {
		t.Fatalf("reminder is missing the raw-source hint: %s", reminder)
	}
	plain := namespaceTestSource("function", "exec_command", "")
	if strings.Contains(clientToolProtocolInstructions(plain), "carries source text") {
		t.Fatal("a function tool without a string code parameter was offered the raw-source form")
	}
	if !supportsFunctionCodeTransport(clientToolSpecs(source)["mcp__cua_repl.js"]) {
		t.Fatal("a function tool with a string code parameter was not recognised")
	}
}

// extended_summary 是上游自己的参数摘要字段：没有 summary 标记时，即使它恰好能解析成 JSON 对象，
// 也不能当"其余参数"用。否则任何带自由文本摘要的普通调用都会被误判（custom 工具会丢参数）。
func TestPlainSummaryIsNotFunctionCodeTransport(t *testing.T) {
	source := functionCodeSource()
	native := map[string]any{
		"type": "function_call", "id": "fc_plain_summary", "call_id": "call_plain_summary", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          "Run client tool mcp__cua_repl.js",
			"extended_summary": `{"timeout_ms":10}`,
			"references":       []any{"mcp__cua_repl.js"},
			"code":             string(jsonBytes(map[string]any{"code": "js()"})),
		})),
	}
	call, err := extractNativeClientToolCall(native, clientToolSpecs(source))
	if err != nil {
		t.Fatalf("a plain call with a free-text summary was rejected: %v", err)
	}
	args := parseArguments(call["arguments"])
	if args["code"] != "js()" {
		t.Fatalf("plain call arguments = %#v", args)
	}
	if _, leaked := args["timeout_ms"]; leaked {
		t.Fatalf("the summary field leaked into the arguments: %#v", args)
	}
	customSource := namespaceTestSource("custom", "apply_patch", "")
	customNative := map[string]any{
		"type": "function_call", "id": "fc_plain_custom", "call_id": "call_plain_custom", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          "Run client tool apply_patch",
			"extended_summary": `{"a":1}`,
			"references":       []any{"apply_patch"},
			"code":             "*** Begin Patch",
		})),
	}
	customCall, err := extractNativeClientToolCall(customNative, clientToolSpecs(customSource))
	if err != nil {
		t.Fatalf("a plain custom call with a free-text summary was rejected: %v", err)
	}
	if customCall["input"] != "*** Begin Patch" {
		t.Fatalf("custom input = %#v", customCall["input"])
	}
}

// ── 测试脚手架 ───────────────────────────────────────────────────────────────

func streamRequest(source map[string]any) ExecutorRequest {
	return ExecutorRequest{
		Model: DefaultModelID, Format: openAIResponseFormat, Stream: true, StreamID: "host-stream",
		AuthMetadata: map[string]any{"access_token": "test-token", "account_id": "test-account"},
		Payload:      jsonBytes(streamPayload(source)),
	}
}

func streamPayload(source map[string]any) map[string]any {
	payload := cloneObject(source)
	payload["model"] = DefaultModelID
	payload["stream"] = true
	payload["store"] = false
	payload["input"] = []any{map[string]any{"role": "user", "content": "hi"}}
	return payload
}

func nonStreamRequest(source map[string]any) ExecutorRequest {
	payload := streamPayload(source)
	payload["stream"] = false
	return ExecutorRequest{
		Model: DefaultModelID, AuthMetadata: map[string]any{"access_token": "test-token", "account_id": "test-account"},
		Payload: jsonBytes(payload),
	}
}

func waitForStream(t *testing.T, state *recordingHostState) {
	t.Helper()
	select {
	case <-state.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the plugin stream was not closed within 10s")
	}
}

type recordingHostState struct {
	mu           sync.Mutex
	chunks       [][]byte
	reads        int
	emitted      []string
	closeError   string
	closed       chan struct{}
	plainBody    []byte
	plainHeaders http.Header
	// readError 让假宿主在 chunk 用尽后以错误收尾；gate 用来把第一次读取卡住，
	// 便于观察上游"一声不响"期间的下游行为。
	readError string
	gate      chan struct{}
}

func (s *recordingHostState) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.emitted...)
}

// newRecordingHost 模拟 CPA 的 host 回调：按顺序吐 chunks，记录每次 emit 与 close。
func newRecordingHost(t *testing.T, chunks ...[]byte) (HostCall, *recordingHostState) {
	t.Helper()
	state := &recordingHostState{chunks: chunks, closed: make(chan struct{}, 1)}
	host := func(method string, payload any, out any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		switch method {
		case "host.http.do":
			return json.Unmarshal(jsonBytes(map[string]any{
				"StatusCode": 200, "Headers": state.plainHeaders, "Body": state.plainBody,
			}), out)
		case "host.http.do_stream":
			return json.Unmarshal(jsonBytes(map[string]any{
				"status_code": 200, "stream_id": "host-stream",
				"headers": map[string][]string{"Content-Type": {"text/event-stream"}},
			}), out)
		case "host.http.stream_read":
			state.mu.Lock()
			index := state.reads
			state.reads++
			gate := state.gate
			state.mu.Unlock()
			if index == 0 && gate != nil {
				<-gate
			}
			if index < len(state.chunks) {
				return json.Unmarshal(jsonBytes(map[string]any{"payload": state.chunks[index], "done": false}), out)
			}
			if state.readError != "" {
				return json.Unmarshal(jsonBytes(map[string]any{"error": state.readError, "done": true}), out)
			}
			return json.Unmarshal(jsonBytes(map[string]any{"done": true}), out)
		case "host.http.stream_close":
			return nil
		case "host.stream.emit":
			var request struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(raw, &request); err != nil {
				return err
			}
			state.mu.Lock()
			state.emitted = append(state.emitted, string(request.Payload))
			state.mu.Unlock()
			return nil
		case "host.stream.close":
			var request struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &request)
			state.mu.Lock()
			state.closeError = request.Error
			state.mu.Unlock()
			select {
			case state.closed <- struct{}{}:
			default:
			}
			return nil
		}
		return fmt.Errorf("unexpected host callback: %s", method)
	}
	return host, state
}
