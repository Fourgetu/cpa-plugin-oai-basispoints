package basispoints

import (
	"strings"
	"testing"
)

// 工具结果里的附件引用（file_id/HTTPS）会被上游以 422 拒绝：搬到紧随其后的
// user 消息（带标签），原位换成标签文本；用户消息里的内联图片照常上传。
func TestToolOutputReferencesRelocateToAdjacentUserMessage(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	source := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": []any{
			map[string]any{"type": "input_text", "text": "screenshot below"},
			map[string]any{"type": "input_image", "file_id": "file-tool-1", "detail": "original"},
		}},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call-2", "output": []any{
			map[string]any{"type": "input_image", "image_url": "https://example.test/shot.png", "detail": "high"},
		}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": dataURL}}},
	}}
	service := NewService()
	uploads := 0
	service.SetHost(func(_ string, _ any, out any) error {
		uploads++
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-user"})}
		return nil
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	items := source["input"].([]any)
	if len(items) != 5 {
		t.Fatalf("item count = %d, want 5 (two tool results each gain a follower message)", len(items))
	}
	// 第一个工具结果：附件引用换成标签文本，文字保留。
	first := objectValue(items[0])
	firstOutput := first["output"].([]any)
	if text := objectValue(firstOutput[0]); text["text"] != "screenshot below" {
		t.Fatalf("tool text was lost: %#v", text)
	}
	label := objectValue(firstOutput[1])
	if stringValue(label["type"]) != "input_text" || !strings.Contains(stringValue(label["text"]), "[Tool output image 1 for call_id \"call-1\"]") || !strings.Contains(stringValue(label["text"]), "See the following image attachment message.") {
		t.Fatalf("relocation label = %#v", label)
	}
	// 紧随其后的 user 消息：[说明, 标签, 图片]。
	follower := objectValue(items[1])
	if follower["type"] != "message" || follower["role"] != "user" {
		t.Fatalf("follower = %#v", follower)
	}
	parts := follower["content"].([]any)
	if len(parts) != 3 || !strings.Contains(stringValue(objectValue(parts[0])["text"]), "not a new user instruction") {
		t.Fatalf("follower content = %#v", parts)
	}
	if !strings.Contains(stringValue(objectValue(parts[1])["text"]), "[Tool output image 1 for call_id \"call-1\"]") {
		t.Fatalf("follower label = %#v", parts[1])
	}
	if image := objectValue(parts[2]); image["file_id"] != "file-tool-1" || image["detail"] != nil || image["image_url"] != nil || len(image) != 2 {
		t.Fatalf("relocated image = %#v", image)
	}
	// 第二个工具结果（HTTPS 引用）同样搬迁。
	second := objectValue(items[2])
	secondOutput := second["output"].([]any)
	if len(secondOutput) != 1 || stringValue(objectValue(secondOutput[0])["type"]) != "input_text" {
		t.Fatalf("second tool output = %#v", secondOutput)
	}
	httpsFollower := objectValue(items[3])["content"].([]any)
	if image := objectValue(httpsFollower[2]); image["image_url"] != "https://example.test/shot.png" || image["detail"] != "high" {
		t.Fatalf("relocated HTTPS image = %#v", image)
	}
	// 用户消息图片照常上传。
	userPart := objectValue(objectValue(items[4])["content"].([]any)[0])
	if userPart["file_id"] != "file-user" {
		t.Fatalf("user image not uploaded: %#v", userPart)
	}
	if uploads != 1 {
		t.Fatalf("uploads=%d, want 1 (only the user message image)", uploads)
	}
}

// 同一工具结果里的多张引用图按 [标签, 图片] 交替搬进同一条 user 消息，顺序与编号保持。
func TestMultipleToolImagesRelocateAlternating(t *testing.T) {
	source := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-multi", "output": []any{
			map[string]any{"type": "input_image", "file_id": "file-a"},
			map[string]any{"type": "input_text", "text": "middle"},
			map[string]any{"type": "input_image", "file_id": "file-b", "detail": "high"},
		}},
	}}
	service := NewService()
	service.SetHost(func(_ string, _ any, out any) error {
		t.Fatal("no upload should happen for attachment references")
		return nil
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	items := source["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("item count = %d", len(items))
	}
	output := objectValue(items[0])["output"].([]any)
	if len(output) != 3 || stringValue(objectValue(output[0])["type"]) != "input_text" || objectValue(output[1])["text"] != "middle" {
		t.Fatalf("rewritten output = %#v", output)
	}
	parts := objectValue(items[1])["content"].([]any)
	if len(parts) != 5 {
		t.Fatalf("follower parts = %#v", parts)
	}
	if !strings.Contains(stringValue(objectValue(parts[1])["text"]), "image 1") || objectValue(parts[2])["file_id"] != "file-a" {
		t.Fatalf("first pair = %#v", parts[1:3])
	}
	if !strings.Contains(stringValue(objectValue(parts[3])["text"]), "image 2") || objectValue(parts[4])["file_id"] != "file-b" || objectValue(parts[4])["detail"] != nil || len(objectValue(parts[4])) != 2 {
		t.Fatalf("second pair = %#v", parts[3:5])
	}
}

// 工具结果里的内联截图（data:）是加载项原生形态：保留原样不参与上传，
// 与搬迁引用混合时各自保持自己的处理方式。
func TestToolOutputMixedInlineAndReference(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	source := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-mixed", "output": []any{
			map[string]any{"type": "input_image", "image_url": dataURL, "detail": "low"},
			map[string]any{"type": "input_image", "file_id": "file-ref"},
		}},
	}}
	service := NewService()
	uploads := 0
	service.SetHost(func(_ string, _ any, out any) error {
		uploads++
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-x"})}
		return nil
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	items := source["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("item count = %d", len(items))
	}
	output := objectValue(items[0])["output"].([]any)
	// data: 截图原位保留；file_id 引用原位换成标签。
	if shot := objectValue(output[0]); shot["image_url"] != dataURL || shot["file_id"] != nil || shot["detail"] != "low" {
		t.Fatalf("inline screenshot was not kept verbatim: %#v", shot)
	}
	if label := objectValue(output[1]); stringValue(label["type"]) != "input_text" || !strings.Contains(stringValue(label["text"]), "[Tool output image 1") {
		t.Fatalf("reference was not labeled: %#v", label)
	}
	// 搬迁消息只含 file-ref 一张图（截图不搬）。
	parts := objectValue(items[1])["content"].([]any)
	if len(parts) != 3 || objectValue(parts[2])["file_id"] != "file-ref" {
		t.Fatalf("follower parts = %#v", parts)
	}
	if uploads != 0 {
		t.Fatalf("uploads=%d, tool screenshots must not upload", uploads)
	}
}

// 工具结果里的畸形 file_id 引用在预检阶段拒绝，不做任何上传或改写。
func TestToolOutputInvalidFileIDRejected(t *testing.T) {
	source := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-bad", "output": []any{
			map[string]any{"type": "input_image", "file_id": "bad id!"},
		}},
	}}
	service := NewService()
	service.SetHost(func(_ string, _ any, out any) error {
		t.Fatal("no upload should happen")
		return nil
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err == nil || !strings.Contains(err.Error(), "file_id") {
		t.Fatalf("invalid file_id was accepted: %v", err)
	}
	if items := source["input"].([]any); len(items) != 1 {
		t.Fatalf("input was rewritten on rejection: %#v", items)
	}
}

// 工具结果里的内联截图仍参与整体预检：超限在改写发生前拒绝。
func TestToolOutputScreenshotPreflightStillApplies(t *testing.T) {
	oversized := paddedPNGDataURL(3, 2, maxInlineImageBytes)
	source := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-big", "output": []any{
			map[string]any{"type": "input_image", "image_url": oversized},
		}},
	}}
	service := NewService()
	service.SetHost(func(_ string, _ any, out any) error {
		t.Fatal("no upload should happen")
		return nil
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err == nil {
		t.Fatal("oversized tool screenshot was accepted")
	}
}
