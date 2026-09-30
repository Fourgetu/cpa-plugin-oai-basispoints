package basispoints

import (
	"strings"
	"testing"
)

// 附件引用只发最小形状 {type, file_id}：上游 v0.2.4 #17（v0.2.5 CHANGELOG）与
// sub2api v2.9.5 的 normalizeMessageFileImages 两侧独立同向。这里钉住三件事：
// ① 无论客户端给了什么 detail / 额外字段，落到 wire 上都只剩下 type 与 file_id；
// ② message 之外的条目（工具结果）与 HTTPS 引用不受影响；
// ③ 归一在校验之后，畸形引用仍要本地报错、不能被最小形状掩盖。
func TestMessageFileReferencesAreMinimized(t *testing.T) {
	for _, detail := range []any{nil, "auto", "low", "high", "original"} {
		for _, kind := range []string{"", "message", "agent_message"} {
			part := map[string]any{
				"type":            "input_image",
				"file_id":         "file-existing",
				"detail":          detail,
				"client_metadata": "private-client-metadata",
			}
			item := map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "before"},
				part,
				map[string]any{"type": "input_text", "text": "after"},
			}}
			if kind != "" {
				item["type"] = kind
			}
			body := map[string]any{"input": []any{item}}
			normalizeFileReferenceImages(body)
			parts := objectValue(body["input"].([]any)[0])["content"].([]any)
			got := objectValue(parts[1])
			if len(got) != 2 || got["type"] != "input_image" || got["file_id"] != "file-existing" {
				t.Fatalf("%s/%v: reference not minimized: %#v", kind, detail, got)
			}
			if objectValue(parts[0])["text"] != "before" || objectValue(parts[2])["text"] != "after" {
				t.Fatalf("%s/%v: neighbouring text changed: %#v", kind, detail, parts)
			}
		}
	}
}

// 工具结果条目与 HTTPS 引用保持原样：前者上游以 422 拒绝其中的附件引用（另有搬迁逻辑），
// 后者的 detail 由本地校验与上游决定，不参与最小形状归一。
func TestFileReferencesOutsideMessagesKeepTheirShape(t *testing.T) {
	body := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": []any{
			map[string]any{"type": "input_image", "file_id": "file-tool", "detail": "original"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "https://example.test/shot.png", "detail": "original"},
			map[string]any{"type": "input_text", "text": "keep"},
		}},
	}}
	normalizeFileReferenceImages(body)
	items := body["input"].([]any)
	tool := objectValue(objectValue(items[0])["output"].([]any)[0])
	if tool["file_id"] != "file-tool" || tool["detail"] != "original" {
		t.Fatalf("tool output reference must stay verbatim: %#v", tool)
	}
	remote := objectValue(objectValue(items[1])["content"].([]any)[0])
	if remote["image_url"] != "https://example.test/shot.png" || remote["detail"] != "original" {
		t.Fatalf("HTTPS image must keep its detail: %#v", remote)
	}
}

// 归一不得掩盖畸形引用：混用两个引用、非法 detail 仍在本地 400，且输入不被改写。
func TestFileReferencesAreValidatedBeforeMinimizing(t *testing.T) {
	for name, part := range map[string]map[string]any{
		"mixed references": {"type": "input_image", "file_id": "file-existing", "image_url": "https://example.test/private-image", "detail": "auto"},
		"invalid detail":   {"type": "input_image", "file_id": "file-existing", "detail": "private-invalid-detail"},
	} {
		t.Run(name, func(t *testing.T) {
			body := contentBody(part)
			service := NewService()
			service.SetHost(func(_ string, _ any, _ any) error {
				t.Fatal("no upload should happen for a rejected reference")
				return nil
			})
			err := service.uploadInputImages(ExecutorRequest{}, body, credential{}, defaultConfig())
			if err == nil {
				t.Fatal("malformed reference was accepted")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatalf("error text leaked the request value: %v", err)
			}
			got := objectValue(objectValue(body["input"].([]any)[0])["content"].([]any)[0])
			if got["file_id"] != part["file_id"] || got["detail"] != part["detail"] || got["image_url"] != part["image_url"] {
				t.Fatalf("rejected reference was rewritten: %#v", got)
			}
		})
	}
}
