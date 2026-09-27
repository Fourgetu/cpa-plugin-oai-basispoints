package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── 命令原文直传（cmd 参数是字符串的 exec_command 一族）────────────────────────

func functionCmdSource() map[string]any {
	return map[string]any{"tools": []any{map[string]any{
		"type": "function", "name": "exec_command",
		"parameters": map[string]any{
			"type": "object", "required": []any{"cmd"},
			"properties": map[string]any{
				"cmd":        map[string]any{"type": "string"},
				"timeout_ms": map[string]any{"type": "integer"},
				"title":      map[string]any{"type": "string"},
			},
		},
	}}}
}

// 形态：summary 是 codex2api.function_cmd/<工具名> 标记，references 指向同一个目录键，
// code 是命令原文，extended_summary 是其余参数的 JSON 对象。
func functionCmdNative(key, command string, metadata any) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_function_cmd", "call_id": "call_function_cmd", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          functionCmdMarker(key),
			"references":       []any{key},
			"extended_summary": metadata,
			"code":             command,
		})),
	}
}

func functionCmdCall(t *testing.T, native map[string]any, source map[string]any) map[string]any {
	t.Helper()
	call, err := extractNativeClientToolCall(native, clientToolSpecs(source))
	if err != nil {
		t.Fatalf("function cmd transport was not decoded: %v", err)
	}
	return call
}

// 命令原文必须一字不差地成为 cmd 参数，其余参数从 extended_summary 合并。
func TestFunctionCmdTransportKeepsCommandVerbatim(t *testing.T) {
	source := functionCmdSource()
	command := "printf \"a b\" | sed -e 's/x/y/g'\nprintf 'line\\t2'\n"
	native := functionCmdNative("exec_command", command, string(jsonBytes(map[string]any{"timeout_ms": 20000, "title": "probe"})))
	call := functionCmdCall(t, native, source)
	if call["type"] != "function_call" || call["name"] != "exec_command" {
		t.Fatalf("client identity = %#v", call)
	}
	if id := stringValue(call["id"]); !strings.HasPrefix(id, itemIDPrefixFunction) {
		t.Fatalf("function item id = %q", id)
	}
	args := parseArguments(call["arguments"])
	if args == nil {
		t.Fatal("merged arguments are not a JSON object")
	}
	if args["cmd"] != command || args["title"] != "probe" || args["timeout_ms"] != json.Number("20000") {
		t.Fatalf("merged arguments = %#v", args)
	}
	if _, _, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source); err != nil || !changed {
		t.Fatalf("transform: changed=%t err=%v", changed, err)
	}
	// 普通 JSON 形状也要继续可用：code 直接放整个参数对象。
	plain := map[string]any{
		"type": "function_call", "id": "fc_plain_cmd", "call_id": "call_plain_cmd", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"references": []any{"exec_command"},
			"code":       string(jsonBytes(map[string]any{"cmd": "pwd"})),
		})),
	}
	if args := parseArguments(functionCmdCall(t, plain, source)["arguments"]); args["cmd"] != "pwd" {
		t.Fatalf("plain JSON shape broke: %#v", args)
	}
}

// 形态不成立时按类别报错：标记指向别的工具、目标不支持、其余参数写坏、命令冲突或缺失。
func TestFunctionCmdTransportRejectsMismatchedPayload(t *testing.T) {
	source := functionCmdSource()
	specs := clientToolSpecs(source)
	codeSpecs := clientToolSpecs(functionCodeSource())
	command := "pwd"

	// 标记指向的工具与 references 声明的目录键不是同一个。
	another := functionCmdNative("exec_command", command, "{}")
	anotherArgs := parseArguments(another["arguments"])
	anotherArgs["references"] = []any{"mcp__cua_repl.js"}
	another["arguments"] = string(jsonBytes(anotherArgs))
	if protocolErr, ok := asProtocolError(mustReject(t, another, codeSpecs)); !ok || !strings.Contains(protocolErr.Message, "function_cmd_marker_names_another_tool") {
		t.Fatalf("marker mismatch: %v", protocolErr)
	}
	// 目标在目录里，但不是 cmd 参数的 exec_command 一族。
	codeTool := functionCmdNative("mcp__cua_repl.js", command, "{}")
	if protocolErr, ok := asProtocolError(mustReject(t, codeTool, codeSpecs)); !ok || !strings.Contains(protocolErr.Message, "function_cmd_marker_requires_cmd_tool") {
		t.Fatalf("unsupported target: %v", protocolErr)
	}
	broken := functionCmdNative("exec_command", command, "{not json")
	if protocolErr, ok := asProtocolError(mustReject(t, broken, specs)); !ok || !strings.Contains(protocolErr.Message, "extended_summary") {
		t.Fatalf("broken metadata: %v", protocolErr)
	}
	conflict := functionCmdNative("exec_command", command, string(jsonBytes(map[string]any{"cmd": "whoami"})))
	if protocolErr, ok := asProtocolError(mustReject(t, conflict, specs)); !ok || !strings.Contains(protocolErr.Message, "function_cmd_duplicates_cmd_in_arguments") {
		t.Fatalf("conflicting duplicate: %v", protocolErr)
	}
	if protocolErr, ok := asProtocolError(mustReject(t, functionCmdNative("exec_command", "", "{}"), specs)); !ok || !strings.Contains(protocolErr.Message, "function_cmd_text_missing") {
		t.Fatalf("missing command: %v", protocolErr)
	}
	// 上游有时把同一条命令重复一遍：只丢掉完全相同的副本，合并结果里没有第二个 cmd。
	duplicated := functionCmdNative("exec_command", command, string(jsonBytes(map[string]any{"cmd": command, "timeout_ms": 5})))
	args := parseArguments(functionCmdCall(t, duplicated, source)["arguments"])
	if args["cmd"] != command || args["timeout_ms"] != json.Number("5") {
		t.Fatalf("identical duplicate was not merged: %#v", args)
	}
	// 目录诊断与解码必须给出同一个类别，方便纠错提示与日志对齐。
	diagnostic := relayDiagnostic(duplicated, specs)
	if diagnostic != "function_cmd_payload_unusable" {
		t.Fatalf("diagnostic for a decodable payload = %q", diagnostic)
	}
}

// 历史回放：这类工具的调用要重新编码成命令原文形态，命令一字不差，并能原样解回来。
func TestFunctionCmdTransportHistoryReplay(t *testing.T) {
	source := functionCmdSource()
	command := "git status --short && echo \"done\"\n"
	call := map[string]any{
		"type": "function_call", "id": "fc_replay_cmd", "call_id": "call_replay_cmd", "name": "exec_command",
		"arguments": string(jsonBytes(map[string]any{"cmd": command, "timeout_ms": json.Number("30000")})),
		"status":    "completed",
	}
	replayed := objectValue(translateInputItems([]any{call}, clientToolSpecs(source))[0])
	if replayed["name"] != transportName {
		t.Fatalf("replay did not use the transport: %#v", replayed)
	}
	outer := parseArguments(replayed["arguments"])
	if outer["code"] != command {
		t.Fatalf("replay lost the raw command: %#v", outer)
	}
	extra, reason := parseRelayObject(outer["extended_summary"])
	if reason != "" || extra["timeout_ms"] != json.Number("30000") {
		t.Fatalf("replay arguments = %#v (reason=%q)", extra, reason)
	}
	if _, duplicated := extra["cmd"]; duplicated {
		t.Fatal("replay duplicated the command inside the arguments object")
	}
	roundTrip := functionCmdCall(t, replayed, source)
	if parseArguments(roundTrip["arguments"])["cmd"] != command {
		t.Fatalf("round trip changed the command: %#v", roundTrip)
	}
}

// 目录与提醒必须教会模型这个形态，且只对 cmd 是字符串的 exec_command 一族生效。
func TestFunctionCmdTransportOnlyForCommandTools(t *testing.T) {
	source := functionCmdSource()
	catalog := clientToolProtocolInstructions(source)
	for _, want := range []string{"exec_command", "put that command directly in code and put its other arguments as one JSON object in extended_summary"} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("catalog is missing %q", want)
		}
	}
	if reminder := clientToolProtocolReminder(source); !strings.Contains(reminder, "exec_command takes a shell command") {
		t.Fatalf("reminder is missing the raw-command hint: %s", reminder)
	}
	if !supportsFunctionCmdTransport(clientToolSpecs(source)["exec_command"]) {
		t.Fatal("an exec_command function with a string cmd parameter was not recognised")
	}
	codeSource := functionCodeSource()
	if supportsFunctionCmdTransport(clientToolSpecs(codeSource)["mcp__cua_repl.js"]) {
		t.Fatal("a tool whose code parameter carries source text was offered the raw-command form")
	}
	plain := functionCmdSource()
	plain["tools"] = []any{map[string]any{
		"type": "function", "name": "exec_command",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cmd": map[string]any{"type": "array"},
			},
		},
	}}
	if supportsFunctionCmdTransport(clientToolSpecs(plain)["exec_command"]) {
		t.Fatal("a non-string cmd parameter was offered the raw-command form")
	}
}

// 纠正只能重排封装，不能换掉命令：绑回后的载荷必须是模型原始发出的字节。
func TestBindRawTransportPayloadsKeepsOriginalCommand(t *testing.T) {
	source := functionCmdSource()
	specs := clientToolSpecs(source)
	command := "rm -rf /tmp/keep-me"
	broken := map[string]any{
		"type": "function_call", "id": "fc_broken", "call_id": "call_broken", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          functionCmdMarker("exec_command"),
			"references":       []any{"exec_command"},
			"extended_summary": "{not json",
			"code":             command,
		})),
	}
	if _, err := extractNativeClientToolCall(broken, specs); err == nil {
		t.Fatal("the broken fixture was expected to fail decoding")
	}
	correctedItem := map[string]any{
		"type": "function_call", "id": "fc_broken", "call_id": "call_broken", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          functionCmdMarker("exec_command"),
			"references":       []any{"exec_command"},
			"extended_summary": string(jsonBytes(map[string]any{"timeout_ms": 1000})),
			"code":             "rm -rf /",
		})),
	}
	corrected := map[string]any{"output": []any{correctedItem}}
	bound := bindRawTransportPayloads([]map[string]any{broken}, corrected, source)
	items, ok := bound["output"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("bound output = %#v", bound["output"])
	}
	entry := objectValue(items[0])
	outer := parseArguments(entry["arguments"])
	if outer["code"] != command {
		t.Fatalf("bound command = %#v, want the original bytes", outer["code"])
	}
	if _, reason := parseRelayObject(outer["extended_summary"]); reason != "" {
		t.Fatalf("binding dropped the corrected metadata: %#v", outer["extended_summary"])
	}
	if entry["call_id"] != "call_broken" || entry["id"] != "fc_broken" {
		t.Fatalf("binding changed the item identity: %#v", entry)
	}
	// 模型响应本体不受影响：历史里记的仍是纠正后的内容。
	if original := stringValue(objectValue(correctedItem)["arguments"]); !strings.Contains(original, "rm -rf /") {
		t.Fatalf("binding rewrote the corrected response in place: %s", original)
	}
	call := functionCmdCall(t, entry, source)
	args := parseArguments(call["arguments"])
	if args["cmd"] != command || args["timeout_ms"] != json.Number("1000") {
		t.Fatalf("bound call arguments = %#v", args)
	}
}

// 纠错验收：换了命令原文的批次一律不采用（而不是带着新命令放行）。
func TestPreservesTransportOperationsRejectsChangedCommand(t *testing.T) {
	source := functionCmdSource()
	before := map[string]any{
		"type": "function_call", "id": "fc_before", "call_id": "call_before", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          functionCmdMarker("exec_command"),
			"references":       []any{"exec_command"},
			"extended_summary": "{not json",
			"code":             "echo safe",
		})),
	}
	after := map[string]any{
		"type": "function_call", "id": "fc_before", "call_id": "call_before", "name": transportName,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          functionCmdMarker("exec_command"),
			"references":       []any{"exec_command"},
			"extended_summary": "{}",
			"code":             "echo changed",
		})),
	}
	if preservesTransportOperations([]map[string]any{before}, []map[string]any{after}, source) {
		t.Fatal("a correction that replaced the command text was accepted")
	}
	same := cloneObject(after)
	sameArgs := parseArguments(same["arguments"])
	sameArgs["code"] = "echo safe"
	same["arguments"] = string(jsonBytes(sameArgs))
	if !preservesTransportOperations([]map[string]any{before}, []map[string]any{same}, source) {
		t.Fatal("a correction that only fixed the envelope was rejected")
	}
}

func mustReject(t *testing.T, native map[string]any, specs map[string]toolSpec) error {
	t.Helper()
	_, err := extractNativeClientToolCall(native, specs)
	if err == nil {
		t.Fatalf("native call was accepted: %#v", native)
	}
	return err
}

// 写坏的载荷要给出类别化诊断（纠错提示与日志按这个类别对齐），且不泄露命令正文。
func TestFunctionCmdTransportDiagnosticIsCategorical(t *testing.T) {
	source := functionCmdSource()
	broken := functionCmdNative("exec_command", "rm -rf /secret-path", "{not json")
	diagnostic := relayDiagnostic(broken, clientToolSpecs(source))
	if !strings.HasPrefix(diagnostic, "extended_summary ") {
		t.Fatalf("diagnostic = %q", diagnostic)
	}
	if strings.Contains(diagnostic, "secret-path") {
		t.Fatalf("diagnostic leaked the command text: %q", diagnostic)
	}
}
