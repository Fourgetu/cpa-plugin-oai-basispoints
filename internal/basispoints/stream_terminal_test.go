package basispoints

// 两项流式终态/回放修复：
// ② 增量桥把 response.cancelled 当终态（此前只认 completed/incomplete/failed/error）：
//    否则上游以 cancelled 收尾时，事件被当普通事件透传，流在没有终态的情况下结束。
// ⑭ 合成流（把已完成响应回放成 SSE）补 reasoning 条目的摘要事件（对齐上游 v0.2.6）：
//    缺了它，客户端"思考"面板在缓冲回放路径上会空白。

import (
	"strings"
	"testing"
)

func TestIncrementalBridgeTreatsCancelledAsTerminal(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	chunks := [][]byte{
		sseEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_cancel", "status": "in_progress", "output": []any{}}}),
		sseEvent("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{
			"type": "function_call", "id": "fc_cancel", "call_id": "call_cancel", "name": "get_weather"}}),
		sseEvent("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_cancel", "delta": `{"city":`}),
		sseEvent("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "msg_cancel", "delta": "partial answer"}),
		sseEvent("response.cancelled", map[string]any{"response": map[string]any{"id": "resp_cancel", "status": "cancelled", "output": []any{
			messageItem("assistant", "partial answer"),
			map[string]any{"type": "function_call", "id": "fc_cancel", "call_id": "call_cancel", "name": "get_weather", "arguments": `{"city":"Oslo"}`},
		}}}),
	}
	host, state := newRecordingHost(t, chunks...)
	service := NewService()
	service.SetHost(host)
	if _, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(source))); err != nil {
		t.Fatalf("a cancelled stream must not surface as a plugin error: %v", err)
	}
	waitForStream(t, state)
	if state.closeError != "" {
		t.Fatalf("stream closed with an error: %q", state.closeError)
	}
	joined := strings.Join(state.snapshot(), "")
	if !strings.Contains(joined, "partial answer") {
		t.Fatalf("already-delivered text was lost: %s", joined)
	}
	if !strings.Contains(joined, "response.cancelled") {
		t.Fatalf("the cancelled terminal was not forwarded: %s", joined)
	}
	if strings.Contains(joined, "response.completed") {
		t.Fatalf("a cancelled stream claimed completion: %s", joined)
	}
	if strings.Contains(joined, "get_weather") {
		t.Fatalf("a cancelled stream leaked a half-finished tool call: %s", joined)
	}
}

func TestSyntheticStreamCarriesReasoningSummary(t *testing.T) {
	first := map[string]any{"type": "summary_text", "text": "first thought"}
	empty := map[string]any{"type": "summary_text", "text": ""}
	response := map[string]any{"id": "resp_reason", "status": "completed", "output": []any{
		map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{first, empty}},
		messageItem("assistant", "done"),
	}}
	events := clientStreamEvents(t, syntheticStream(response))
	added, delta, textDone, partDone := 0, 0, 0, 0
	for _, event := range events {
		switch stringValue(event["type"]) {
		case "response.output_item.added":
			item := objectValue(event["item"])
			if stringValue(item["type"]) != "reasoning" {
				continue
			}
			if summary, ok := item["summary"].([]any); !ok || len(summary) != 0 {
				t.Fatalf("reasoning item.added must start with an empty summary: %#v", item["summary"])
			}
		case "response.reasoning_summary_part.added":
			added++
			if part := objectValue(event["part"]); stringValue(part["text"]) != "" {
				t.Fatalf("part.added must announce an empty part: %#v", part)
			}
		case "response.reasoning_summary_text.delta":
			delta++
			if stringValue(event["delta"]) != "first thought" {
				t.Fatalf("unexpected reasoning delta %q", event["delta"])
			}
		case "response.reasoning_summary_text.done":
			textDone++
		case "response.reasoning_summary_part.done":
			partDone++
		}
	}
	if added != 2 || partDone != 2 || delta != 1 || textDone != 2 {
		t.Fatalf("reasoning summary events: added=%d delta=%d textDone=%d partDone=%d", added, delta, textDone, partDone)
	}
}

// 没有摘要的推理条目也必须能被回放（只发生命周期，不发空摘要事件）。
func TestSyntheticStreamHandlesReasoningWithoutSummary(t *testing.T) {
	response := map[string]any{"id": "resp_reason_empty", "status": "completed", "output": []any{
		map[string]any{"type": "reasoning", "id": "rs_2", "summary": []any{}},
		messageItem("assistant", "done"),
	}}
	events := clientStreamEvents(t, syntheticStream(response))
	for _, event := range events {
		if strings.HasPrefix(stringValue(event["type"]), "response.reasoning_summary") {
			t.Fatalf("an empty summary emitted %q", stringValue(event["type"]))
		}
	}
}
