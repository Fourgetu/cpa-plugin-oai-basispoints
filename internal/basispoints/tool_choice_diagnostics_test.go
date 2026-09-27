package basispoints

import (
	"strings"
	"testing"
)

// 诊断要区分"工具未声明"与"声明了但本轮 tool_choice 不允许"：
// 两类都是协议错误，但纠错提示回灌给模型时，"目录里没有"会误导模型换工具而不是等 tool_choice 放开。
func TestToolChoiceDiagnosticSeparatesCatalogAndChoice(t *testing.T) {
	source := namespaceTestSource("function", "get_weather", "")
	source["tool_choice"] = "none"
	declared := clientToolSpecs(source)
	callable := callableClientToolSpecs(source)
	if _, ok := declared["get_weather"]; !ok {
		t.Fatal("get_weather must be declared")
	}
	if _, ok := callable["get_weather"]; ok {
		t.Fatal("get_weather must not be callable under tool_choice=none")
	}
	native := namespaceTestNative("choice", "get_weather", map[string]any{"city": "Tokyo"})

	if _, err := extractNativeClientToolCallWithCatalog(native, callable, declared); err == nil || !strings.Contains(err.Error(), "tool_not_allowed_by_tool_choice") {
		t.Fatalf("tool_choice rejection = %v", err)
	}
	if diagnostic := relayDiagnosticWithCatalog(native, callable, declared); !strings.HasPrefix(diagnostic, "tool_not_allowed_by_tool_choice:") {
		t.Fatalf("diagnostic = %q", diagnostic)
	}
	// 主路径（终态转换）也要给出同一类别，让重生成/纠错提示回灌准确的原因。
	_, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
	if err == nil || !strings.Contains(err.Error(), "tool_not_allowed_by_tool_choice:get_weather") {
		t.Fatalf("transform error = %v", err)
	}

	// 真未声明的工具仍然是 tool_not_in_catalog。
	unknown := namespaceTestNative("unknown", "no_such_tool", map[string]any{"city": "Tokyo"})
	if _, err := extractNativeClientToolCallWithCatalog(unknown, callable, declared); err == nil || !strings.Contains(err.Error(), "tool_not_in_catalog") {
		t.Fatalf("unknown tool rejection = %v", err)
	}
	if diagnostic := relayDiagnosticWithCatalog(unknown, callable, declared); !strings.HasPrefix(diagnostic, "tool_not_in_catalog:") {
		t.Fatalf("unknown tool diagnostic = %q", diagnostic)
	}
}

// allowed_tools 只放行部分工具时，未放行的声明工具给出 tool_choice 类别。
func TestToolChoiceAllowedToolsDiagnostic(t *testing.T) {
	source := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "alpha", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "beta", "parameters": map[string]any{"type": "object"}},
	}, "tool_choice": map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{
		map[string]any{"type": "function", "name": "alpha"},
	}}}
	declared := clientToolSpecs(source)
	callable := callableClientToolSpecs(source)
	if len(declared) != 2 || len(callable) != 1 {
		t.Fatalf("declared=%d callable=%d", len(declared), len(callable))
	}
	beta := namespaceTestNative("beta", "beta", map[string]any{"x": 1})
	if _, err := extractNativeClientToolCallWithCatalog(beta, callable, declared); err == nil || !strings.Contains(err.Error(), "tool_not_allowed_by_tool_choice") {
		t.Fatalf("beta rejection = %v", err)
	}
	if _, err := extractNativeClientToolCallWithCatalog(namespaceTestNative("alpha", "alpha", map[string]any{"x": 1}), callable, declared); err != nil {
		t.Fatalf("alpha must stay callable: %v", err)
	}
}
