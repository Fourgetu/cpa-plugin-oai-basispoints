package basispoints

import (
	"fmt"
	"testing"
)

// 多代理协作历史里的 agent_message 也可能带内联图片：与用户消息同样上传成附件引用；
// agent_message 即便带 assistant 角色字段也不受"assistant 消息跳过"规则影响。
func TestAgentMessageInlineImagesAreUploaded(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	source := map[string]any{"input": []any{
		map[string]any{"type": "agent_message", "role": "assistant", "content": []any{
			map[string]any{"type": "input_text", "text": "screenshot from the helper"},
			map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"},
		}},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "input_image", "image_url": dataURL}}},
	}}
	service := NewService()
	uploads := 0
	service.SetHost(func(_ string, _ any, out any) error {
		uploads++
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-agent-message"})}
		return nil
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	items := source["input"].([]any)
	agentParts := objectValue(items[0])["content"].([]any)
	part := objectValue(agentParts[1])
	if part["file_id"] != "file-agent-message" || part["image_url"] != nil || part["detail"] != "high" {
		t.Fatalf("agent_message image not rewritten: %#v", part)
	}
	if text := objectValue(agentParts[0]); text["text"] != "screenshot from the helper" {
		t.Fatalf("agent_message text changed: %#v", text)
	}
	assistant := objectValue(objectValue(items[1])["content"].([]any)[0])
	if assistant["image_url"] != dataURL || assistant["file_id"] != nil {
		t.Fatal("plain assistant message image was uploaded")
	}
	if uploads != 1 {
		t.Fatalf("uploads=%d, want 1", uploads)
	}
}

// agent_message 的图片同样参与整请求预检：超限在任意上传发生前拒绝。
func TestAgentMessageImagesJoinPreflight(t *testing.T) {
	oversized := paddedPNGDataURL(3, 2, maxInlineImageBytes)
	source := map[string]any{"input": []any{
		map[string]any{"type": "agent_message", "content": []any{imagePart(oversized)}},
	}}
	service := NewService()
	uploads := 0
	service.SetHost(func(_ string, _ any, out any) error {
		uploads++
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": fmt.Sprintf("file-%d", uploads)})}
		return nil
	})
	err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig())
	if err == nil {
		t.Fatal("oversized agent_message image was accepted")
	}
	if uploads != 0 {
		t.Fatalf("uploads=%d, preflight must reject before any upload", uploads)
	}
}
