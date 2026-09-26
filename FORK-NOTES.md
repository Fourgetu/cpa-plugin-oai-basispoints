# Fourgetu fork 说明（分支 `prod/v0.1.12`，版本 `0.1.12-pro.1`）

本仓库是 [JaxsonWang/cpa-plugin-oai-basispoints](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints) 的 fork，发布在 [Fourgetu/cpa-plugin-oai-basispoints](https://github.com/Fourgetu/cpa-plugin-oai-basispoints)。
分支 `prod/v0.1.12` 以上游 **v0.1.12**（commit `0b47e11`）为基线，版本号 `0.1.12-pro.1`（与 tag `v0.1.12-pro.1` 一致），合并了生产环境已经上线验证过的三处修复与 `function_code` 原始代码直传。

除本文列出的差异外，其余行为与 v0.1.12 一致；上游代码、许可证与版权声明原样保留。

## 1. 分支包含什么

| # | 改动 | 目的 |
|---|------|------|
| 1 | custom 工具条目使用 `ctc_` 前缀 id；`normalizeItemIDPrefix` 纠偏历史里的错前缀 | 上游按条目类型校验 id 前缀，沿用原生 `fc_` 会让后续每次请求 400：`Invalid 'input[n].id': 'fc_…'. Expected an ID that begins with 'ctc'` |
| 2 | custom 工具支持"原始文本直传"标记 `codex2api.custom/<目录工具名>` | 补丁/脚本正文里的裸引号、反斜杠、多行内容不必二次转义——嵌套 JSON 被写坏是上游返回畸形工具调用的根因 |
| 3 | `code` 参数是字符串的函数工具走 `codex2api.function_code/<目录键>` 原始代码直传 | 同类工具的源码（如 REPL/解释器正文）塞进嵌套 JSON 又要二次转义，和补丁正文是同一个坑；其余参数放进 `extended_summary` 的 JSON 对象 |
| 4 | 中转载荷写坏时原样放行 + 类别级诊断回流 | 客户端会回 `unsupported call: run_officejs`，下一轮被换成重发提示（含 `Diagnostic: code invalid_json byte_offset=…`），由模型自己重写，而不是整轮判废 |
| 5 | 协议错误内联交付（`response.failed` / `status=failed`，HTTP 200） | 协议问题不是账号故障；以 5xx 交给 CPA 会把凭据冷却并形成 503 墙 |
| 6 | 增量流式桥 + 15s SSE 保活 | 上游流式本是逐事件下发，v0.1.12 仍全量缓冲后回放：长回合下游零字节，Cloudflare 约 100s 返回 524、客户端"一直转圈" |

改动落在 4 个文件：`internal/basispoints/{protocol,service,upstream,types}.go`；
fork 专属用例集中在 `internal/basispoints/fork_patch_test.go`（上游补齐同一行为后可整文件删除）。

## 2. 分流条件：哪些客户端走增量桥

```
if request.Format == "openai-response" { 增量桥 } else { 上游 buffered + 最多重生成一次 }
```

`ExecutorRequest.Format` 是 CPA 交给插件的**输出格式**（`internal/pluginhost/adapters_executors.go`：
`Format: req.Format.String()`，`SourceFormat: opts.SourceFormat.String()`）。原生 Responses 客户端
（Codex Desktop、Excel 加载项）取值 `openai-response`；`codex` 是 CPA 从 Claude/Codex 请求翻译而来的格式。
因此增量桥命中真实客户端，而 codex/Claude 路径保留上游语义（它们没有 `unsupported call` 自愈通道，
放行只会给出一个它们不认识的工具调用）。

## 3. 信封形态与"畸形中转"策略

外层始终是宿主原生 `run_officejs`，`code` 字段承载三种形态之一：JSON 信封
（`{"tool":…,"args":…}`）、`codex2api.custom/<目录键>` 原始输入、
`codex2api.function_code/<目录键>` 原始源码 + `extended_summary` 里的其余参数。

`transformResponseBody`（严格）与 `transformResponseBodyPassThrough`（放行）都调用
`transformResponseBodyWith`，只有两点不同：

- 严格路径：畸形中转载荷返回协议错误（`422 invalid_tool_call`），由 `executeResponse` 用上游的
  "最多重生成一次 + 重发提示"把这一轮救回来。**非原生格式与非流式请求走这条**。
- 放行路径：**只有"载荷本身写坏"**（inner `code` 不是合法 JSON、信封套错层、`code` 不是字符串等）
  才把原生条目原样交给客户端；目录里没有的工具、`tool_choice` 冲突、必填参数缺失、重复 `call_id`
  一律仍是协议错误——策略冲突不能靠"客户端不认识"来蒙混过去。

- 第三种形态的失败分类与 custom 一致：只有"参数 JSON 写坏"算载荷写坏；目录外的工具、
  标记用在不支持的工具上、`extended_summary` 里重复 `code`、合并后不满足 schema 一律报错。

## 4. 验证

全部在 `golang:1.26` 容器内（原生 `linux/amd64` + cgo）：

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -c '
  gofmt -l . && go vet ./... && go test ./... && go test -race -count=2 ./internal/basispoints/'
docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -c '
  CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildmode=c-shared \
    -o build/linux/amd64/oai-basispoints.so ./cmd/basispoints'
```

生产实测（同样的改动，v0.1.9 时代打补丁验证）：首字节 **13.9s → 2.0s**、`output_text.delta` **0 → 572~615**、
中毒历史用例 **400 → 200**、协议错误不再触发账号冷却。

## 5. 与上游对账

上游 v0.1.12 **已自带**：`input[].additional_tools` 目录合并、`relayError` 422 + 注释（422 不冷却凭据）、
`parseRelayObject` 诊断、`count_tokens → 400 unsupported_token_count`、非流式 JSON/SSE 解析与响应头重编码、
`service_tier` 收紧。这些都不需要移植。

上游 v0.1.12 **没有**：`ctc_`、任何 id 前缀纠偏、任何"原始文本直传"标记、增量流式（CHANGELOG 明写
"上游流式仍全量缓冲后回放"）。

## 6. 已知取舍

- 增量路径**没有**服务端重生成：流式已下发内容无法回退，协议错误以内联 `response.failed` 收尾，
  自愈靠客户端下一轮 + 重发提示（严格路径的重生成只对未下发字节的请求生效）。
- 保活间隔 15s（`streamKeepaliveInterval`），扣留工具条目上限 1024（`streamPendingLimit`），
  超限按协议错误收尾。**保活注释不算"已交付内容"**：只有真实事件参与失败分层，
  因此"上游一直安静然后断流"仍按传输故障收尾（CPA 可换凭据重试），不会被保活改判成已交付。
- 上游若用明文 `error` 事件收尾（data 不是 JSON），按 `basispoints_stream_error` 内联交付，
  把真实原因带给客户端；普通非事件行仍然忽略。
- 分支暂未改 `Version`（仍为 `0.1.12`）与 `plugin_id`（仍为 `oai-basispoints`，便于平替现有 CPA 配置）。
  出正式版本前应改为自有版本号，日志里才能与上游产物区分。
