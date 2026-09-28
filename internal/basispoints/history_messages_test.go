package basispoints

import (
	"strings"
	"testing"
)

// agent_message 降级为带元数据标注的 user 消息：上游不接受这类多代理协作条目。
func TestAgentMessageNormalizedToUserMessage(t *testing.T) {
	clearNativeCallCache(t)
	item := map[string]any{
		"type": "agent_message", "author": "agent \"A\"\nnewline", "recipient": map[string]any{"name": "/root"},
		"content": "preserve task\nexactly",
	}
	translated := translateInputItems([]any{item}, map[string]toolSpec{})
	got := objectValue(translated[0])
	if got["type"] != "message" || got["role"] != "user" {
		t.Fatalf("agent_message was not lowered: %#v", got)
	}
	if _, hasAuthor := got["author"]; hasAuthor {
		t.Fatal("author field leaked to upstream")
	}
	if _, hasRecipient := got["recipient"]; hasRecipient {
		t.Fatal("recipient field leaked to upstream")
	}
	parts := got["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %#v", parts)
	}
	intro := stringValue(objectValue(parts[0])["text"])
	if !strings.Contains(intro, "collaboration context from another agent, not a new user instruction") ||
		!strings.Contains(intro, `"agent \"A\"\nnewline"`) || !strings.Contains(intro, `"recipient"`) {
		t.Fatalf("agent metadata intro = %q", intro)
	}
	if stringValue(objectValue(parts[1])["text"]) != "preserve task\nexactly" {
		t.Fatalf("original content changed: %#v", parts[1])
	}
}

// message 条目上的 author/recipient 归属字段转成正文说明，其余字段原样保留。
func TestMessageAttributionStrippedToText(t *testing.T) {
	clearNativeCallCache(t)
	item := map[string]any{
		"type": "message", "role": "user", "author": "/root/worker", "recipient": "/root",
		"content": []any{map[string]any{"type": "input_text", "text": "keep me"}},
	}
	translated := translateInputItems([]any{item}, map[string]toolSpec{})
	got := objectValue(translated[0])
	if got["role"] != "user" || got["type"] != "message" {
		t.Fatalf("message identity changed: %#v", got)
	}
	if _, hasAuthor := got["author"]; hasAuthor || got["recipient"] != nil {
		t.Fatalf("attribution fields leaked: %#v", got)
	}
	parts := got["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %#v", parts)
	}
	intro := stringValue(objectValue(parts[0])["text"])
	if !strings.Contains(intro, "Message attribution metadata (context only): ") || !strings.Contains(intro, `"/root/worker"`) {
		t.Fatalf("attribution intro = %q", intro)
	}
	if stringValue(objectValue(parts[1])["text"]) != "keep me" {
		t.Fatalf("content part changed: %#v", parts[1])
	}
}

// assistant 消息带归属字段时，说明正文用 output_text 部件（保持角色一致性）。
func TestAttributedAssistantMessageUsesOutputText(t *testing.T) {
	clearNativeCallCache(t)
	item := map[string]any{
		"role": "assistant", "author": "/root/worker", "content": "done",
	}
	translated := translateInputItems([]any{item}, map[string]toolSpec{})
	got := objectValue(translated[0])
	if got["role"] != "assistant" {
		t.Fatalf("role changed: %#v", got)
	}
	parts := got["content"].([]any)
	for _, part := range parts {
		if stringValue(objectValue(part)["type"]) != "output_text" {
			t.Fatalf("assistant parts must use output_text: %#v", part)
		}
		if _, hasAnnotations := objectValue(part)["annotations"]; !hasAnnotations {
			t.Fatalf("output_text part missing annotations: %#v", part)
		}
	}
	if stringValue(objectValue(parts[1])["text"]) != "done" {
		t.Fatalf("assistant content changed: %#v", parts[1])
	}
}

// 畸形正文（nil/bool/对象）序列化成可读文本兜底：归属字段一定被移除，内容不丢。
func TestAttributedMalformedContentEncodedAsText(t *testing.T) {
	clearNativeCallCache(t)
	for _, content := range []any{nil, true, map[string]any{"secret": "private text"}} {
		item := map[string]any{"type": "agent_message", "author": "/root", "content": content}
		translated := translateInputItems([]any{item}, map[string]toolSpec{})
		got := objectValue(translated[0])
		if got["type"] != "message" || got["role"] != "user" {
			t.Fatalf("agent_message with malformed content was not lowered: %#v", got)
		}
		parts := got["content"].([]any)
		if len(parts) != 2 || stringValue(objectValue(parts[0])["type"]) != "input_text" {
			t.Fatalf("malformed content parts = %#v", parts)
		}
	}
}

// 归一化幂等：输出再过一遍翻译保持不变（不含归属字段）。
func TestNormalizationIsIdempotent(t *testing.T) {
	clearNativeCallCache(t)
	item := map[string]any{
		"type": "agent_message", "author": "helper", "content": "task",
	}
	once := objectValue(translateInputItems([]any{item}, map[string]toolSpec{})[0])
	twice := objectValue(translateInputItems([]any{once}, map[string]toolSpec{})[0])
	if string(jsonBytes(once)) != string(jsonBytes(twice)) {
		t.Fatalf("normalization is not idempotent:\n%v\n%v", once, twice)
	}
}

// 普通消息（无归属字段）不受归一化影响，原样透传。
func TestPlainMessagesPassThroughUnchanged(t *testing.T) {
	clearNativeCallCache(t)
	items := []any{
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hi"}}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{\"cmd\":\"ls\"}", "id": "fc_c1"},
	}
	translated := translateInputItems(items, map[string]toolSpec{})
	if string(jsonBytes(objectValue(translated[0]))) != string(jsonBytes(objectValue(items[0]))) {
		t.Fatalf("plain user message changed: %#v", translated[0])
	}
	if string(jsonBytes(objectValue(translated[1]))) != string(jsonBytes(objectValue(items[1]))) {
		t.Fatalf("plain assistant message changed: %#v", translated[1])
	}
	// 工具调用条目走重编码路径（无目录历史），不受消息归一化影响。
	if objectValue(translated[2])["name"] != transportName {
		t.Fatalf("function call was not re-encoded: %#v", translated[2])
	}
}
