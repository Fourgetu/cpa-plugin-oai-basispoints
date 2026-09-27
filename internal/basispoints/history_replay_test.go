package basispoints

import (
	"reflect"
	"strings"
	"testing"
)

func clearNativeCallCache(t *testing.T) {
	t.Helper()
	nativeCallCache.Lock()
	nativeCallCache.items = map[string]map[string]any{}
	nativeCallCache.order = nil
	nativeCallCache.Unlock()
}

// 历史轮调用过、但本轮目录没有声明的工具（压缩请求不带 tools、目录变化或原生缓存逐出）时，
// 客户端格式的历史调用也必须重编码成中转信封——原样透传会被上游直接拒绝。
func TestHistoryReplayWithoutCurrentToolCatalog(t *testing.T) {
	cases := []struct {
		name    string
		item    map[string]any
		wantRef string
		wantRaw string
	}{
		{
			name: "function",
			item: map[string]any{
				"type": "function_call", "id": "fc_hist", "call_id": "call_hist", "name": "exec_command",
				"arguments": string(jsonBytes(map[string]any{"cmd": "pwd"})), "status": "completed",
			},
			wantRef: "exec_command",
			wantRaw: string(jsonBytes(map[string]any{"cmd": "pwd"})),
		},
		{
			name: "custom",
			item: map[string]any{
				"type": "custom_tool_call", "id": "ctc_hist", "call_id": "call_hist", "name": "apply_patch",
				"input": "*** Begin Patch\n+hello\n", "status": "completed",
			},
			wantRef: "apply_patch",
			wantRaw: "*** Begin Patch\n+hello\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearNativeCallCache(t)
			translated := translateInputItems([]any{tc.item}, map[string]toolSpec{})
			replayed := objectValue(translated[0])
			if replayed["name"] != transportName {
				t.Fatalf("uncatalogued history was passed through verbatim: %#v", replayed)
			}
			outer := parseArguments(replayed["arguments"])
			if !reflect.DeepEqual(outer["references"], []any{tc.wantRef}) {
				t.Fatalf("references = %#v, want [%s]", outer["references"], tc.wantRef)
			}
			if outer["code"] != tc.wantRaw {
				t.Fatalf("code = %#v, want the original payload verbatim", outer["code"])
			}
			if outer["summary"] == "" || outer["summary"] == functionCodeMarker(tc.wantRef) || outer["summary"] == functionCmdMarker(tc.wantRef) {
				t.Fatalf("uncatalogued replay must use the generic envelope, got summary=%#v", outer["summary"])
			}
		})
	}
}

// 目录存在但工具声明被移除时同样要重编码；配对的工具结果统一改写成 function_call_output。
func TestHistoryReplayPairingWithoutCatalogTool(t *testing.T) {
	clearNativeCallCache(t)
	call := map[string]any{
		"type": "function_call", "id": "fc_hist", "call_id": "call_hist", "name": "exec_command",
		"arguments": string(jsonBytes(map[string]any{"cmd": "pwd"})), "status": "completed",
	}
	output := map[string]any{
		"type": "function_call_output", "id": "fco_hist", "call_id": "call_hist", "output": "done",
	}
	translated := translateInputItems([]any{call, output}, map[string]toolSpec{})
	replayed := objectValue(translated[0])
	if replayed["name"] != transportName {
		t.Fatalf("call was not re-encoded: %#v", replayed)
	}
	paired := objectValue(translated[1])
	if paired["type"] != "function_call_output" {
		t.Fatalf("paired output type = %#v, want function_call_output", paired["type"])
	}
	if paired["output"] != "done" || paired["call_id"] != "call_hist" {
		t.Fatalf("paired output content changed: %#v", paired)
	}
	if _, hasName := paired["name"]; hasName {
		t.Fatal("paired output kept a client tool name")
	}
}

// 原生缓存被逐出（或服务重启后冷缓存）不影响历史重编码。
func TestHistoryReplayAfterNativeCacheEviction(t *testing.T) {
	clearNativeCallCache(t)
	source := namespaceTestSource("function", "exec_command", "")
	specs := clientToolSpecs(source)
	// 先走一轮"原生条目记忆"路径，确认缓存生效。
	native := namespaceTestNative("evict", "exec_command", map[string]any{"cmd": "ls"})
	translated := translateInputItems([]any{native}, specs)
	if objectValue(translated[0])["call_id"] != "call_evict" {
		t.Fatalf("native replay changed identity: %#v", translated[0])
	}
	if rememberedNativeCall("call_evict") == nil {
		t.Fatal("native call was not remembered")
	}
	// 清空缓存后，客户端格式历史（此前由原生条目记忆路径覆盖）仍要重编码。
	clearNativeCallCache(t)
	clientCall := map[string]any{
		"type": "function_call", "id": "fc_evict", "call_id": "call_evict", "name": "exec_command",
		"arguments": string(jsonBytes(map[string]any{"cmd": "ls"})), "status": "completed",
	}
	translated = translateInputItems([]any{clientCall}, map[string]toolSpec{})
	replayed := objectValue(translated[0])
	if replayed["name"] != transportName {
		t.Fatalf("cold-cache client history was passed through: %#v", replayed)
	}
}

// 钉死"不静默删除或改写未知的历史条目"：上游注入的原生类型（mcp_call、local_shell_call、
// web_search_call、computer_call、compaction_trigger 等）原样保留，只做内部标记剥离与
// id 前缀纠偏。这一行为此前靠约定维持，现在用测试锁住。
func TestUnknownHistoryItemTypesPassThrough(t *testing.T) {
	clearNativeCallCache(t)
	items := []any{
		map[string]any{"type": "mcp_call", "id": "mc_1", "name": "ctx.read", "arguments": "{}"},
		map[string]any{"type": "local_shell_call", "id": "sh_1", "action": map[string]any{"command": "ls"}},
		map[string]any{"type": "web_search_call", "id": "ws_1", "action": map[string]any{"query": "x"}},
		map[string]any{"type": "computer_call", "id": "cc_1", "action": map[string]any{"type": "click"}},
		map[string]any{"type": "compaction_trigger", "id": "cpt_1"},
		map[string]any{"type": "future_unknown_kind", "id": "zz_1", "payload": map[string]any{"keep": true}},
	}
	translated := translateInputItems(items, map[string]toolSpec{})
	if len(translated) != len(items) {
		t.Fatalf("history length changed: %d -> %d", len(items), len(translated))
	}
	for index, value := range items {
		got := objectValue(translated[index])
		want := objectValue(value)
		if got["type"] != want["type"] || got["id"] != want["id"] {
			t.Fatalf("item %d was rewritten: %#v", index, got)
		}
		if name, hasName := want["name"]; hasName && got["name"] != name {
			t.Fatalf("item %d lost its name: %#v", index, got)
		}
		if payload, hasPayload := want["payload"]; hasPayload && !reflect.DeepEqual(got["payload"], payload) {
			t.Fatalf("item %d lost its payload: %#v", index, got)
		}
	}
	if strings.Contains(string(jsonBytes(translated)), transportName) {
		t.Fatal("unknown history items must not be re-encoded as transport calls")
	}
}
