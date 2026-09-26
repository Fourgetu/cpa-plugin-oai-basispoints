# Fourgetu fork 说明（分支 `prod/v0.1.14`，版本 `0.1.14-pro.1`）

本仓库是 [JaxsonWang/cpa-plugin-oai-basispoints](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints) 的 fork，发布在 [Fourgetu/cpa-plugin-oai-basispoints](https://github.com/Fourgetu/cpa-plugin-oai-basispoints)。
分支 `prod/v0.1.14` 以上游 **v0.1.14**（commit `1b9359a`）为基线，版本号 `0.1.14-pro.1`（与 tag 一致），在上游之上只保留五处生产环境验证过的改动。

除本文列出的差异外，其余行为与 v0.1.14 一致；上游代码、MIT 许可证与版权声明原样保留。

## 1. 与上游的关系（v0.1.14 起收敛）

上游 v0.1.14 自己解决了我们此前用标记绕过的两件事，因此本分支**丢弃**了旧的私有标记，改用上游信封：

- 上游已把中继信封改成 **`references: [完整目录工具名]` 路由 + `code` 只放该工具载荷**（function 工具 = 参数 JSON 对象，custom 工具 = 逐字原文），并修好了 message 流式的 `content_part.*` 事件序列。
- 我们的旧标记 `codex2api.custom/<工具名>`（写在 `summary` 里）随之删除；`codex2api.function_code/<工具名>` 只保留为"原始代码直传"的形态标记（第 3 节第 3 种形态）。

## 2. 本分支保留的五处改动

| # | 改动 | 目的 |
|---|------|------|
| 1 | custom 工具条目使用 `ctc_` 前缀 id；`normalizeItemIDPrefix` 纠偏历史里的错前缀 | 上游按条目类型校验 id 前缀，沿用原生 `fc_` 会让后续每次请求 400：`Invalid 'input[n].id': 'fc_…'. Expected an ID that begins with 'ctc'`。上游 v0.1.14 仍未修。 |
| 2 | 中转载荷写坏时原样放行 + 类别级诊断回流 | 客户端会回 `unsupported call: run_officejs`，下一轮被换成重发提示（含 `Diagnostic: code invalid_json byte_offset=…`），由模型自己重写，而不是整轮判废 |
| 3 | 协议错误内联交付（`response.failed` / `status=failed`，HTTP 200） | 协议问题不是账号故障；以 5xx 交给 CPA 会把凭据冷却并形成 503 墙 |
| 4 | 增量流式桥 + 15s SSE 保活 | 上游 v0.1.14 仍是"全量缓冲后回放"，长回合下游零字节会被 Cloudflare 524；本分支文本事件到达即下发 |
| 5 | `code` 参数是字符串的函数工具走"原始代码直传"（`summary` 标记 + `code` 源码 + `extended_summary` 其余参数） | 例如 `mcp__cua_repl.js(code, timeout_ms, title)`：源码按普通函数形状塞进 `code` 的 JSON 对象里要二次转义；约定沿用 hloolx/codex2api，经 ranxi2001/sub2api 的 BPS 协议包对照 |

改动落在 5 个文件：`internal/basispoints/{protocol,service,upstream,types}.go`（fork 独有代码集中在 `protocol.go` 末尾的补丁段）；
fork 专属用例集中在 `internal/basispoints/fork_patch_test.go`（上游补齐同一行为后可整文件删除）。

## 3. 信封形态

外层始终是宿主原生 `run_officejs`，`references` 恰好一个目录工具名，`code` 承载该工具的载荷：

- **function 工具（默认）**：`code` 是一个 JSON 参数对象，例如 `{"cmd":"pwd"}`；
- **custom 工具**：`code` 是逐字保留的原始文本（补丁正文里的引号、反斜杠、换行不再需要二次转义）；
- **function 工具 + `code` 参数声明为 string（本分支第 5 条）**：`summary` 放形态标记 `codex2api.function_code/<完整工具名>`，
  `code` 放源码原文，其余参数作为一个 **JSON 对象**放在 `extended_summary`。

第三种形态靠 `summary` 里的标记判别，不能靠内容猜：上游自己的外层参数就带一个 `extended_summary`（自然语言的调用摘要），
模型几乎每条调用都会写；若按"`extended_summary` 能不能解析成 JSON 对象"判别，普通调用会被误判成这种形态、custom 工具的参数会被悄悄丢掉。
标记必须指向 `references` 里那个工具、且该工具 schema 声明了 string 类型的 `code`，否则按契约冲突报错（不放行）；
没有标记时一律按第一种形态解析 `code`。模型写哪种形态都行，两条解码路径都有用例覆盖。

## 4. 分流条件：哪些客户端走增量桥

```
if request.Format == "openai-response" { 增量桥 } else { 上游 buffered + 最多重生成一次 }
```

`ExecutorRequest.Format` 是 CPA 交给插件的**输出格式**（`internal/pluginhost/adapters_executors.go`：`Format: req.Format.String()`）。
原生 Responses 客户端（Codex Desktop、Excel 加载项）取值 `openai-response`；`codex` 是 CPA 从 Claude/Codex 请求翻译而来的格式。
因此增量桥命中真实客户端，而 codex/Claude 路径保留上游语义（它们没有 `unsupported call` 自愈通道）。

## 5. 两种"畸形中转"策略

`transformResponseBody`（严格）与 `transformResponseBodyPassThrough`（放行）都调用 `transformResponseBodyWith`：

- 严格路径：畸形载荷返回协议错误（`422 invalid_tool_call`），由 `executeResponse` 用上游的"最多重生成一次 + 重发提示"
  把这一轮救回来。**非原生格式与非流式请求走这条**（与上游 v0.1.14 行为一致，含其 `legacy_*` 兼容用例）。
- 放行路径：**只有"载荷本身写坏"**（`code` 不是合法 JSON、外层 arguments 写坏、标记形态下的 `extended_summary` 不是 JSON 对象）才把原生条目
  原样交给客户端；目录外的工具、`tool_choice` 冲突、必填参数缺失、schema 不符、重复 `call_id` 一律仍是协议错误。

## 6. 验证

全部在 `golang:1.26` 容器内（原生 `linux/amd64` + cgo）：

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -c '
  gofmt -l . && go mod tidy -diff && go vet ./... && go test ./... && go test -race -count=2 ./internal/basispoints/'
docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -c '
  CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildmode=c-shared \
    -o build/linux/amd64/oai-basispoints.so ./cmd/basispoints'
```

生产实测（同样的改动，v0.1.9 时代打补丁验证）：首字节 **13.9s → 2.0s**、`output_text.delta` **0 → 572~615**、
中毒历史用例 **400 → 200**、协议错误不再触发账号冷却。

## 7. 已知取舍

- 增量路径**没有**服务端重生成：流式已下发内容无法回退，协议错误以内联 `response.failed` 收尾，
  自愈靠客户端下一轮 + 重发提示（严格路径的重生成只对未下发字节的请求生效）。
- 保活间隔 15s（`streamKeepaliveInterval`），扣留工具条目上限 1024（`streamPendingLimit`）。
  **保活注释不算"已交付内容"**：只有真实事件参与失败分层，因此"上游一直安静然后断流"仍按传输故障收尾。
- 上游若用明文 `error` 事件收尾（data 不是 JSON），按 `basispoints_stream_error` 内联交付，把真实原因带给客户端。
- **旧格式历史**：模型可能从历史里照抄旧的 `{tool,args}` 信封（上游 v0.1.14 提示词已提醒不要照抄）。
  严格路径下它会被拒并重生成一次；原生 Responses 路径下会被原样放行、由客户端回 `unsupported call` 后自愈。
- 上游 v0.1.12 及更早的历史在 `prod/v0.1.12` 分支保留，便于对比与回退。
- 形态标记占用 `summary` 字段：这类调用的摘要显示的是标记串而不是自然语言（与 codex2api/sub2api 的约定一致，便于双方回放彼此写下的历史）。
