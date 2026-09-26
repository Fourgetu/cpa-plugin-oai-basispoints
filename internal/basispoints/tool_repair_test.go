package basispoints

// 工具封装纠错的用例：边界、操作保全、纠错请求构造、桥接器端到端。
// 设计对齐 ranxi2001/sub2api v2.8.15（PR #96），落地细节见 protocol.go 末尾的补丁段。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func repairResponse(items ...any) map[string]any {
	return map[string]any{"id": "resp_repair", "status": "completed", "model": DefaultUpstreamModel, "output": items}
}

func sseBlock(joined, event string) string {
	for _, block := range strings.Split(joined, "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(block), "event: "+event) {
			return block
		}
	}
	return ""
}

func sseBlockData(block string) string {
	if index := strings.Index(block, "data: "); index >= 0 {
		return block[index+len("data: "):]
	}
	return ""
}

// 只有"清一色原生 run_officejs 调用、外层参数可解析"的批次才允许追问纠错。
func TestRepairableTransportBatchEligibility(t *testing.T) {
	functionSource := namespaceTestSource("function", "get_weather", "")
	customSource := namespaceTestSource("custom", "apply_patch", "")
	valid := namespaceTestNative("ok", "get_weather", map[string]any{"city": "Tokyo"})
	broken := brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	for _, tc := range []struct {
		name   string
		source map[string]any
		resp   map[string]any
		want   bool
	}{
		{"malformed-payload", functionSource, repairResponse(valid, broken), true},
		{"all-valid", functionSource, repairResponse(valid), true},
		{"custom-raw-payload", customSource, repairResponse(brokenRelayFixture("apply_patch", "*** Begin Patch")), true},
		{"incomplete-status", functionSource, map[string]any{"status": "incomplete", "output": []any{broken}}, false},
		{"no-tools", functionSource, repairResponse(messageItem("assistant", "hi")), false},
		{"foreign-function", functionSource, repairResponse(map[string]any{
			"type": "function_call", "call_id": "call_foreign", "name": "read_file", "arguments": "{}"}), false},
		{"duplicate-call-id", functionSource, repairResponse(broken, broken), false},
		{"missing-call-id", functionSource, repairResponse(map[string]any{
			"type": "function_call", "name": transportName, "arguments": `{"references":["get_weather"],"code":"{}"}`}), false},
		{"unparsable-outer-arguments", functionSource, repairResponse(map[string]any{
			"type": "function_call", "call_id": "call_bad", "name": transportName, "arguments": "not json"}), false},
		{"out-of-catalog-target", functionSource, repairResponse(brokenRelayFixture("not_in_catalog", "x")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, ok := repairableTransportBatch(tc.resp, tc.source)
			if ok != tc.want {
				t.Fatalf("eligible = %t (items=%d), want %t", ok, len(items), tc.want)
			}
		})
	}
}

// 纠错后的批次必须逐条保住原本要执行的操作。
func TestTransportOperationPreservation(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	customSource := namespaceTestSource("custom", "apply_patch", "")
	valid := namespaceTestNative("a", "get_weather", map[string]any{"city": "Tokyo"})
	if !preservesTransportOperations([]map[string]any{valid},
		[]map[string]any{namespaceTestNative("b", "get_weather", map[string]any{"city": "Tokyo"})}, source) {
		t.Fatal("an unchanged operation was rejected")
	}
	if preservesTransportOperations([]map[string]any{valid},
		[]map[string]any{namespaceTestNative("c", "get_weather", map[string]any{"city": "Oslo"})}, source) {
		t.Fatal("a changed operation was accepted")
	}
	// 函数工具的 code 允许重写：把源码错写进 code 只能靠重写成参数 JSON 修好。
	if !preservesTransportOperations([]map[string]any{brokenRelayFixture("get_weather", "raw source text")},
		[]map[string]any{namespaceTestNative("d", "get_weather", map[string]any{"city": "Tokyo"})}, source) {
		t.Fatal("a legitimate function payload rewrite was rejected")
	}
	// custom 的裸文本必须逐字保留。
	if !preservesTransportOperations([]map[string]any{brokenRelayFixture("apply_patch", "*** Begin Patch\n+line\n")},
		[]map[string]any{brokenRelayFixture("apply_patch", "*** Begin Patch\n+line\n")}, customSource) {
		t.Fatal("an unchanged custom payload was rejected")
	}
	if preservesTransportOperations([]map[string]any{brokenRelayFixture("apply_patch", "*** Begin Patch\n+line\n")},
		[]map[string]any{brokenRelayFixture("apply_patch", "*** Begin Patch\n+other\n")}, customSource) {
		t.Fatal("a rewritten custom payload was accepted")
	}
}

// 纠错请求：模型、会话、历史与 metadata 原样保留，只追加"这批没执行"的回灌与纠正提示。
func TestToolRepairRequestFeedback(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	broken := brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	prepared := map[string]any{
		"model":    "gpt-6-astra",
		"input":    []any{messageItem("user", "weather?")},
		"metadata": map[string]any{"agent_iteration": "2", "turn_id": "turn-1"},
	}
	validation := errors.New("Basis Points returned an invalid client tool relay: code invalid_json byte_offset=1")
	request, err := buildToolRepairRequest(prepared, repairResponse(broken), source, validation)
	if err != nil {
		t.Fatalf("repair request was not built: %v", err)
	}
	if stringValue(request["model"]) != "gpt-6-astra" {
		t.Fatalf("model changed: %v", request["model"])
	}
	input, _ := request["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input length = %d, want 4 (history + failed output + feedback + hint)", len(input))
	}
	feedback := objectValue(input[2])
	if stringValue(feedback["type"]) != "function_call_output" || stringValue(feedback["call_id"]) != stringValue(broken["call_id"]) {
		t.Fatalf("feedback item = %#v", feedback)
	}
	payload, reason := parseRelayObject(feedback["output"])
	if reason != "" || payload["executed"] != false || stringValue(objectValue(payload["error"])["code"]) != toolRepairFeedbackCode {
		t.Fatalf("feedback payload = %#v", payload)
	}
	content, _ := objectValue(input[3])["content"].([]any)
	hint := ""
	if len(content) > 0 {
		hint = stringValue(objectValue(content[0])["text"])
	}
	if !strings.Contains(hint, "exactly 1 run_officejs calls") {
		t.Fatalf("developer hint = %q", hint)
	}
	metadata := objectValue(request["metadata"])
	if stringValue(metadata["agent_iteration"]) != "3" || stringValue(metadata["turn_id"]) != "turn-1" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if stringValue(objectValue(prepared["metadata"])["agent_iteration"]) != "2" {
		t.Fatal("the prepared request metadata was mutated")
	}
	if _, err := buildToolRepairRequest(prepared, repairResponse(namespaceTestNative("ok", "get_weather", map[string]any{"city": "Tokyo"})), source, validation); err == nil {
		t.Fatal("a fully valid batch was accepted for correction")
	}
	if _, err := buildToolRepairRequest(prepared, repairResponse(broken), source, nil); err == nil {
		t.Fatal("a correction request without a validation error was accepted")
	}
}

// 载荷写坏时先追问一次：文本不重放、工具条目用纠正后的、用量合并、响应身份不变。
func TestStreamRepairsMalformedRelayBatch(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	broken := brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	fixed := namespaceTestNative("fixed", "get_weather", map[string]any{"city": "Tokyo"})
	var mu sync.Mutex
	var emitted []string
	attempts := 0
	bridge := newToolStreamBridge(func(payload []byte) error {
		mu.Lock()
		emitted = append(emitted, string(payload))
		mu.Unlock()
		return nil
	}, source)
	bridge.setRepair(func(failed map[string]any, validation error) (map[string]any, error) {
		attempts++
		if validation == nil {
			t.Error("a correction was requested without a validation error")
		}
		return map[string]any{
			"id": "resp_corrected", "status": "completed", "model": DefaultUpstreamModel,
			"output": []any{messageItem("assistant", "corrected commentary"), fixed},
			"usage":  map[string]any{"input_tokens": json.Number("7"), "total_tokens": json.Number("10")},
		}, nil
	})
	feed := func(event string, payload map[string]any) {
		t.Helper()
		if err := bridge.handleEvent(event, jsonBytes(payload)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	feed("response.created", map[string]any{"response": map[string]any{"id": "resp_repair", "status": "in_progress"}})
	feed("response.output_text.delta", map[string]any{"delta": "working"})
	feed("response.output_item.added", map[string]any{"output_index": 1, "item": broken})
	feed("response.output_item.done", map[string]any{"output_index": 1, "item": broken})
	feed("response.completed", map[string]any{"response": map[string]any{
		"id": "resp_repair", "status": "completed", "model": DefaultUpstreamModel,
		"output": []any{messageItem("assistant", "working"), broken},
		"usage":  map[string]any{"input_tokens": json.Number("11"), "total_tokens": json.Number("13")},
	}})
	if attempts != 1 {
		t.Fatalf("correction attempts = %d, want 1", attempts)
	}
	mu.Lock()
	joined := strings.Join(emitted, "")
	mu.Unlock()
	if strings.Contains(joined, "corrected commentary") {
		t.Fatal("intermediate correction text was forwarded to the client")
	}
	if !strings.Contains(joined, "working") {
		t.Fatal("the original streamed text was lost")
	}
	if count := strings.Count(joined, "event: response.completed"); count != 1 {
		t.Fatalf("completed events = %d, want 1", count)
	}
	var completed map[string]any
	if err := json.Unmarshal([]byte(sseBlockData(sseBlock(joined, "response.completed"))), &completed); err != nil {
		t.Fatalf("completed payload: %v", err)
	}
	response := objectValue(completed["response"])
	if stringValue(response["id"]) != "resp_repair" {
		t.Fatalf("response identity changed: %v", response["id"])
	}
	output, _ := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed output length = %d, want 2", len(output))
	}
	repairedItem := objectValue(output[1])
	arguments, reason := parseRelayObject(repairedItem["arguments"])
	if reason != "" || stringValue(arguments["city"]) != "Tokyo" {
		t.Fatalf("repaired tool item = %#v", repairedItem)
	}
	if usage := objectValue(response["usage"]); fmt.Sprint(usage["total_tokens"]) != "23" {
		t.Fatalf("merged usage = %#v, want total_tokens 23", usage)
	}
}

// 两次都没改对：回落到原有的放行自愈路径，而不是把这一轮判废。
func TestStreamRepairFallsBackToPassthrough(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	broken := brokenRelayFixture("get_weather", `{"city":"To"kyo"}`)
	var mu sync.Mutex
	var emitted []string
	attempts := 0
	bridge := newToolStreamBridge(func(payload []byte) error {
		mu.Lock()
		emitted = append(emitted, string(payload))
		mu.Unlock()
		return nil
	}, source)
	bridge.setRepair(func(failed map[string]any, validation error) (map[string]any, error) {
		attempts++
		return repairResponse(broken), nil
	})
	feed := func(event string, payload map[string]any) {
		t.Helper()
		if err := bridge.handleEvent(event, jsonBytes(payload)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	feed("response.created", map[string]any{"response": map[string]any{"id": "resp_repair", "status": "in_progress"}})
	feed("response.output_item.done", map[string]any{"output_index": 0, "item": broken})
	feed("response.completed", map[string]any{"response": map[string]any{
		"id": "resp_repair", "status": "completed", "output": []any{broken},
	}})
	if attempts != maxToolRepairs {
		t.Fatalf("correction attempts = %d, want %d", attempts, maxToolRepairs)
	}
	mu.Lock()
	joined := strings.Join(emitted, "")
	mu.Unlock()
	if count := strings.Count(joined, "event: response.completed"); count != 1 {
		t.Fatalf("completed events = %d, want 1", count)
	}
	if !strings.Contains(joined, transportName) {
		t.Fatal("the original native item was not passed through for the next-turn self-heal")
	}
	if strings.Contains(joined, "response.failed") {
		t.Fatal("a repairable batch must not end the turn with a local failure")
	}
}

// 失败回合尽量沿用上游终态的身份与用量。
func TestFailureStreamKeepsTerminalIdentity(t *testing.T) {
	terminal := map[string]any{
		"id": "resp_real", "model": DefaultUpstreamModel,
		"usage": map[string]any{"input_tokens": json.Number("11"), "total_tokens": json.Number("13")},
	}
	stream := string(syntheticFailureStream(terminal, "", "basispoints_protocol_error", "boom"))
	if !strings.Contains(stream, `"resp_real"`) || !strings.Contains(stream, `"total_tokens":13`) {
		t.Fatalf("failure stream lost the terminal identity or usage: %s", stream)
	}
	if !strings.Contains(stream, `"status":"failed"`) {
		t.Fatalf("failure stream is not a failed response: %s", stream)
	}
}
