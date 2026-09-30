package basispoints

// 上游 v0.2.8 与 sub2api v2.9.5 独立修了同一处：中继示例不能写死工具名与形状。
// 我们此前把 exec_command（函数）与 apply_patch（custom 裸文本）两个示例无条件写进所有请求的提示词，
// 目录里没有该工具、或该工具被声明成另一种类型时，会教模型发错名字或错形状。

import (
	"strings"
	"testing"
)

func toolCatalogSource(tools ...map[string]any) map[string]any {
	list := make([]any, 0, len(tools))
	for _, tool := range tools {
		list = append(list, tool)
	}
	return map[string]any{"tools": list}
}

func execCommandFunction() map[string]any {
	return map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
		"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"},
	}}
}

func applyPatchCustom() map[string]any {
	return map[string]any{"type": "custom", "name": "apply_patch"}
}

func applyPatchFunction() map[string]any {
	return map[string]any{"type": "function", "name": "apply_patch", "parameters": map[string]any{
		"type": "object", "properties": map[string]any{"patch": map[string]any{"type": "string"}}, "required": []any{"patch"},
	}}
}

func weatherFunction() map[string]any {
	return map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"},
	}}
}

// 目录里没有这两个工具时，提示词里不能再出现它们（旧实现会无条件写死）。
func TestRelayExamplesNeverTeachToolsOutsideTheCatalog(t *testing.T) {
	text := clientToolProtocolInstructions(toolCatalogSource(weatherFunction()))
	for _, forbidden := range []string{"exec_command", "apply_patch"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("catalog without %s still teaches it: %s", forbidden, text)
		}
	}
}

// 目录里有它们时，示例按各自声明的类型给出：函数给 JSON 参数对象，custom 给原始文本。
func TestRelayExamplesFollowTheDeclaredType(t *testing.T) {
	text := clientToolProtocolInstructions(toolCatalogSource(execCommandFunction(), applyPatchCustom()))
	if !strings.Contains(text, "Example outer arguments for exec_command (function)") {
		t.Fatalf("function example missing: %s", text)
	}
	if !strings.Contains(text, "Example outer arguments for apply_patch (custom)") {
		t.Fatalf("custom example missing: %s", text)
	}
	if !strings.Contains(text, `\"cmd\":\"printf`) {
		t.Fatalf("function example is not a JSON arguments object: %s", text)
	}
}

// 同一个名字被声明成函数时，示例必须给 {"patch": ...} 的 JSON 对象，而不是裸 patch 文本。
func TestRelayExamplesRespectFunctionShapedApplyPatch(t *testing.T) {
	text := clientToolProtocolInstructions(toolCatalogSource(applyPatchFunction()))
	if !strings.Contains(text, "Example outer arguments for apply_patch (function)") {
		t.Fatalf("function-shaped patch example missing: %s", text)
	}
	if !strings.Contains(text, `\"patch\":\"*** Begin Patch`) {
		t.Fatalf("patch example is not wrapped in its declared argument object: %s", text)
	}
	if strings.Contains(text, "apply_patch (custom)") {
		t.Fatalf("a function tool was demonstrated as custom: %s", text)
	}
}

// 命名空间里的工具用完整限定名（references 的取值就是它）。
func TestRelayExamplesUseTheQualifiedName(t *testing.T) {
	namespaced := map[string]any{"type": "namespace", "name": "functions", "tools": []any{applyPatchCustom()}}
	text := clientToolProtocolInstructions(toolCatalogSource(namespaced))
	if !strings.Contains(text, "Example outer arguments for functions.apply_patch (custom)") {
		t.Fatalf("qualified name was not used: %s", text)
	}
}

// 参数结构对不上声明 schema 的工具不生成示例（宁可不教，也不教错）。
func TestRelayExamplesSkipToolsWhoseSchemaDoesNotMatch(t *testing.T) {
	odd := map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{
		"type": "object", "properties": map[string]any{"command": map[string]any{"type": "object"}}, "required": []any{"command"},
	}}
	text := clientToolProtocolInstructions(toolCatalogSource(odd))
	if strings.Contains(text, "Example outer arguments") {
		t.Fatalf("an example was emitted for a tool whose schema rejects it: %s", text)
	}
}

// 两层 JSON 的说明在协议说明、纠错提示、每轮提醒三处共用同一措辞（对齐上游 v0.2.8）。
func TestFunctionRelayEncodingIsSharedAcrossPrompts(t *testing.T) {
	marker := "These are two distinct JSON layers"
	instructions := clientToolProtocolInstructions(toolCatalogSource(execCommandFunction()))
	if !strings.Contains(instructions, marker) {
		t.Fatalf("protocol instructions lost the two-layer wording: %s", instructions)
	}
	if !strings.Contains(transportRetryHint, marker) {
		t.Fatalf("retry hint lost the two-layer wording: %s", transportRetryHint)
	}
	reminder := clientToolProtocolReminder(toolCatalogSource(execCommandFunction()))
	if !strings.Contains(reminder, marker) {
		t.Fatalf("per-round reminder lost the two-layer wording: %s", reminder)
	}
}
