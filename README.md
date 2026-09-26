# CPA OpenAI Basis Points 插件

> 本仓库是 [Fourgetu](https://github.com/Fourgetu) 的 fork：基线为上游 v0.1.14，版本号 `0.1.14-pro.1`。相对上游的改动、分流条件与取舍见 [FORK-NOTES.md](FORK-NOTES.md)，更新日志见 [CHANGELOG.md](CHANGELOG.md)。上游代码、MIT 许可证与版权声明原样保留。
这是一个 CLIProxyAPI（CPA）原生插件，用 CPA 已有的 ChatGPT/Codex OAuth 凭据直接请求。

## 通过 CPA 插件商店安装（推荐）

在管理界面的「第三方插件源 → 插件源 registry URL (plugins.store-sources)」中添加以下地址并保存，然后刷新插件商店，搜索 **CPA OpenAI Basis Points**：

```text
https://raw.githubusercontent.com/Fourgetu/cpa-plugin-oai-basispoints/v0.1.14-pro.1/registry.json
```

也可合并到 CPA **宿主配置**（`config.yaml`，与下方插件配置共用同一个 `plugins` 节点）：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/Fourgetu/cpa-plugin-oai-basispoints/v0.1.14-pro.1/registry.json
```

保留已有插件源，不要整体覆盖原有 `plugins` 配置；内置官方源由 CPA 自动保留。本源使用宿主原生的 `github-release` 安装方式，最新版本以本仓库已发布的 GitHub Release 为准，不在 registry 中另行维护版本号。CPA 会按运行平台下载 `oai-basispoints_<version>_<goos>_<goarch>.zip`，并使用同一 Release 的 `checksums.txt` 校验。本仓库的 `main` 分支就是本 fork 的默认分支，内容与最新 tag 一致（只多文档更新）；想跟随最新代码也可以用 `https://raw.githubusercontent.com/Fourgetu/cpa-plugin-oai-basispoints/main/registry.json`，上面的 tag 地址则用于钉住具体版本。

发行包覆盖 Linux、macOS、Windows 的 AMD64/ARM64。插件商店负责下载、校验和安装；更新已加载的动态库后仍需重启 CPA，使新代码及 OAuth 认证解析生效。

## 与上游的差异

基线是上游 [JaxsonWang/cpa-plugin-oai-basispoints](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints) 的 `v0.1.14`（commit `1b9359a`）。本分支只在其之上做下面这些改动，上游代码、MIT 许可证与版权声明原样保留；逐条取舍、复现方式与生产实测数据见 [FORK-NOTES.md](FORK-NOTES.md)，版本历史见 [CHANGELOG.md](CHANGELOG.md)。

| # | 改动 | 为什么需要 |
|---|------|-----------|
| 1 | custom 工具条目改用 `ctc_` 前缀 id，并用 `normalizeItemIDPrefix` 纠偏历史里残留的错前缀 | 上游按条目类型校验 id 前缀；custom 条目沿用原生 `fc_` 会让后续每次请求 400（`Invalid 'input[n].id': 'fc_…'. Expected an ID that begins with 'ctc'`），一条坏历史污染整段对话 |
| 2 | 中转载荷**本身写坏**时把原生条目原样放行给原生 Responses 客户端，并把类别级诊断回流到下一轮的重发提示 | 上游对这类输入整轮判废；放行后客户端会回 `unsupported call: run_officejs`，下一轮模型自己按诊断改写，不必重发整轮 |
| 3 | 协议错误内联交付（流内 `response.failed`、非流式 `status=failed`，HTTP 200） | 协议问题是模型/客户端的问题，不是账号故障；以 5xx 交给 CPA 会把凭据冷却，形成"503 墙" |
| 4 | 原生 Responses 客户端（`Format == "openai-response"`）走增量流式桥 + 15s SSE 保活 | 上游是"先读完上游 SSE 再整体回放"，长回合下游零字节会被 Cloudflare 524；本分支文本事件到达即下发（实测首字节 13.9s → 2.0s、`output_text.delta` 0 → 572~615） |
| 5 | `code` 参数声明为 string 的函数工具支持"原始代码直传"：`summary` 放 `codex2api.function_code/<工具名>` 标记、`code` 放源码原文、其余参数作为一个 JSON 对象放 `extended_summary` | 这类工具（如 `mcp__cua_repl.js`）的正文是源码，按普通函数形状塞进 `code` 的 JSON 对象里要二次转义；形态约定沿用 hloolx/codex2api（经 ranxi2001/sub2api 的 BPS 协议包对照） |

此外还有两处收紧，属于本分支对放行策略的加固：

- 放行条目同样参与 `call_id` 唯一性校验，否则下一轮历史里两个同名工具结果无从配对。
- 形态判别只认 `summary` 里的 `codex2api.function_code/<工具名>` 标记：上游自己的 `extended_summary` 是自然语言调用摘要、模型几乎每条调用都会带，拿"它能不能解析成 JSON 对象"当判据会把普通调用误判成这种形态，custom 工具的参数会被悄悄丢掉。

**按上游保留、本分支不改的**：v0.1.14 的中继信封（`references: [完整工具名]` 路由 + `code` 只承载该工具载荷、custom 原文直传）、message 流式的 `content_part.*` 事件序列、以及"非法调用最多重生成一次"的策略。

## 安装和配置

1. 将 `build/linux/amd64/oai-basispoints.so` 复制到 CPA 的 Linux amd64 插件目录。
2. 将 `config.example.yaml` 按需合并到 CPA 的 `config.yaml`；它是完整的宿主配置示例，不会由插件自动读取。插件内置默认暴露 `gpt-6-astra-basispoints`，示例同时配置 Astra 和 Sol，可继续增删模型。
3. CPA 的 `auth-dir` 中已有的 `type: codex` OAuth 文件会被插件识别；插件只在内存中读取 token，不生成另一份 token 文件。
4. 客户端使用 Responses 协议调用 `gpt-6-astra-basispoints`。模型目录声明图像输入，以及 `low`、`medium`、`high`、`xhigh`、`max`、`ultra` 思考等级；`max` 映射为 `xhigh`，`ultra` 原样传递，未指定时默认 `medium`。

插件的 `auth.parse` 会接管 CPA 中 `type: codex` 的 OAuth 文件，并为同一个文件展开两条内存认证：一条保留原生 `codex`，另一条是 `oai-basispoints` 虚拟认证。这样现有 Codex 模型继续使用 CPA 原生执行器，`gpt-6-astra-basispoints` 则使用本插件；不会生成或改写 OAuth 文件。原生 Codex 记录保留源 OAuth 元数据，供原生执行器读取访问令牌和刷新令牌。注意：当前 CPA 会把这两条记录都标记为虚拟认证，不持久化原生记录的刷新结果；Basis Points 记录也不会自动同步原生记录在内存中刷新的 JWT。源 JWT 过期时，需要先通过 CPA 更新或重新导入源 OAuth 凭据，再重新加载，单纯重载过期文件无效。流式响应遵循 Responses SSE 格式，但为保证工具调用可在完整 item 上做安全转换，当前会先读完上游 SSE 再回放给客户端，不是 token 级实时转发。

## 构建

```bash
make test
make build
```

## 协议边界

- 上游请求始终带 `Authorization: Bearer <access_token>`、`chatgpt-account-id`、`x-openai-account-id` 和 `x-basispoints-auth-mode: chatgpt`。
- `turn_id` 按会话和当前用户 turn 稳定生成；工具结果回合只递增 `agent_iteration`，不会把同一 turn 重新当成新计划。
- 工具中继通过外层 `references: [完整工具名]` 路由，`code` 只承载该工具的载荷：function 工具为参数 JSON 对象，custom 工具为逐字保留的原始文本。不要再套 `{tool,args}` 内层包装；插件不执行其中代码。已有会话的原生历史调用原样回放，新调用按本次注入的协议生成。
- 非法函数 JSON、目录外工具或不符合 schema 的参数仍严格拒绝，最多重新生成一次，失败返回 422；不猜测修补引号、不丢弃坏调用，也不将失败响应部分交付。
- 工具条目 id：custom 调用使用 `ctc_` 前缀，历史里残留的错误前缀按条目类型自动纠偏（否则上游会以 400 拒绝整段历史）。
- 协议错误内联交付：以流内 `response.failed`（非流式 `status=failed`、HTTP 200）返回，不做 5xx 让 CPA 冷却凭据。
- 原生 Responses 客户端（`Format == "openai-response"`）走增量流式：文本到达即下发、工具事件扣留到终态校验后合成、上游安静时每 15s 发送 SSE 保活注释；`codex` 等其它格式仍走全量缓冲 + 最多重生成一次。
- `code` 参数声明为 string 的函数工具支持"原始代码直传"：`summary` 放 `codex2api.function_code/<工具名>` 形态标记，`code` 放源码原文，其余参数作为一个 JSON 对象放在 `extended_summary`（没有该标记时一律按普通函数形状解析 `code`）。
- 未能从 OAuth JWT 或凭据字段得到账号 ID、token 过期、上游返回非 2xx、工具名不在客户端目录中时，插件会报告明确错误，不伪造成功。

---

## 版权与社区支持

本项目基于 [MIT License](LICENSE) 开源

感谢 [LINUX DO 社区](https://linux.do/) 的支持

<a href="https://linux.do/">
  <img src="docs/assets/linuxdo.png" alt="LINUX DO 社区" width="360" />
</a>
