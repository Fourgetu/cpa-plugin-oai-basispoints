package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
)

const (
	Version        = "0.2.2-pro.7"
	Provider       = "oai-basispoints"
	AuthProviderID = "codex"
	PluginID       = Provider

	DefaultResponsesURL  = "https://bps.openai.com/basispoints/api/responses"
	DefaultUpstreamModel = "gpt-6-astra"
	DefaultModelID       = "gpt-6-astra-basispoints"

	// openAIResponseFormat 是 CPA 交给原生 Responses 客户端（Codex Desktop、Excel 加载项）
	// 的输出格式；codex 等其它格式是 CPA 从 Claude/Codex 请求翻译而来的。
	openAIResponseFormat = "openai-response"
)

// failProtocol 描述"这一轮无法按客户端协议交付"的本地问题：上游返回的工具调用与客户端目录
// 对不上、条目结构畸形、流没有正常收尾等。它不是账号或凭据故障，因此不能作为 5xx 交给 CPA
// ——那会让 CPA 把账号判为故障并冷却，之后同一账号的所有请求一起撞 503。
// 422 与上游 relayError 同码：CPA 不会因此冷却凭据，由服务层决定是否内联交付。
func failProtocol(code, message string) error {
	return fail(422, code, message)
}

// asProtocolError 判断错误是不是本地协议问题（而不是账号、传输或上游故障）。
// 只有白名单里的类别才算：未知类别一律保留上游的硬错误行为，避免把真实故障内联化。
func asProtocolError(err error) (*APIError, bool) {
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError == nil || apiError.Status != 422 {
		return nil, false
	}
	switch apiError.Kind {
	case "invalid_tool_call", "basispoints_protocol_error", "basispoints_invalid_response",
		"basispoints_stream_incomplete", "basispoints_stream_error":
		return apiError, true
	}
	return nil, false
}

// asUpstreamServerError 判断错误是不是上游 5xx（含传输层失败）。单凭据插件里把这些交给 CPA 会把账号
// 冷却、整个模型变成 503 墙（同一账号的其它客户端一起断），所以这类错误在服务层按内联失败交付。
// `upstream_transport` / `attachment_transport` 是 host.http.do 一层的失败（2026-09-30 补）：传输问题
// 同样不是凭据故障，冷却唯一凭据只会把可恢复的抖动放大成整个别名的 503。
func asUpstreamServerError(err error) (*APIError, bool) {
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError == nil || apiError.Status < 500 || apiError.Status > 599 {
		return nil, false
	}
	switch apiError.Kind {
	case "upstream_error", "attachment_upload_error", "upstream_transport", "attachment_transport":
		return apiError, true
	}
	return nil, false
}

// asUpstreamModelAccessError 判断错误是不是"上游拒绝该模型/账号"（404 模型不存在/无访问权，或
// 403 模型访问变更、权限、配额类）。2026-09-30 实测：上游对某个模型回 404（`The model \`…\` does not
// exist or you do not have access to it.`，其内部还会把模型名贴上 `degrade2-luna` / `codex-abuse` 一类
// 标签），CPA 收到后会把这套（单凭据插件的唯一）凭据冷却成持续的 503 `auth_unavailable`、且不会自愈
// ——只能重启 CPA。这类错误重试与冷却都无意义，因此与上游 5xx 同样按内联失败交付。
// **401（凭据过期/无效）刻意不在此列**：那确实是凭据问题，该让 CPA 与运维看见。
func asUpstreamModelAccessError(err error) (*APIError, bool) {
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError == nil {
		return nil, false
	}
	if apiError.Status != 404 && apiError.Status != 403 {
		return nil, false
	}
	switch apiError.Kind {
	case "upstream_error", "attachment_upload_error":
	default:
		return nil, false
	}
	if !isModelAccessDenial(apiError.Message) {
		return nil, false
	}
	return apiError, true
}

// isModelAccessDenial 识别上游"模型不存在 / 无访问权 / 模型访问已变更"的文案或标识符。
// 文案之外也认 `upstream_code=` / `upstream_type=` 里的已知标识（上游 v0.2.7 与 sub2api v2.9.5 也是按
// 标识符分类），因为同一条语义可能只有 code、没有那段英文。其它 4xx（responses_url 配错、内容策略拒绝
// 等）仍原样交给 CPA，保持配置错误足够响亮。
func isModelAccessDenial(message string) bool {
	lowered := strings.ToLower(message)
	for _, marker := range []string{
		"does not exist or you do not have access",
		"model_not_found",
		"model access has changed",
		"basispoints_model_access_changed",
		"upstream_code=permission_error",
		"upstream_code=permission_denied",
		"upstream_code=insufficient_quota",
		"upstream_code=usage_limit_reached",
		"upstream_type=permission_error",
		"upstream_type=permission_denied",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// protocolFailureCode 给内联失败选错误码（协议错误自身带的类别优先）。
func protocolFailureCode(err error) string {
	var apiError *APIError
	if errors.As(err, &apiError) && apiError != nil && strings.TrimSpace(apiError.Kind) != "" {
		return apiError.Kind
	}
	return "basispoints_protocol_error"
}

var supportedReasoningEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {}, "ultra": {},
}

// APIError carries a downstream HTTP status through the CPA plugin envelope.
type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *APIError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

func (e *APIError) Code() string {
	if e == nil || e.Kind == "" {
		return "plugin_error"
	}
	return e.Kind
}

func fail(status int, kind, message string) error {
	return &APIError{Status: status, Kind: kind, Message: message}
}

type HostCall func(method string, payload any, out any) error

// ExecutorRequest mirrors CPA's JSON executor contract. HTTPClient is not part
// of the JSON ABI; this plugin deliberately uses the host callback instead.
type ExecutorRequest struct {
	AuthID          string            `json:"AuthID"`
	AuthProvider    string            `json:"AuthProvider"`
	Model           string            `json:"Model"`
	Format          string            `json:"Format"`
	Stream          bool              `json:"Stream"`
	Alt             string            `json:"Alt"`
	Headers         http.Header       `json:"Headers"`
	Query           url.Values        `json:"Query"`
	OriginalRequest []byte            `json:"OriginalRequest"`
	SourceFormat    string            `json:"SourceFormat"`
	Payload         []byte            `json:"Payload"`
	Metadata        map[string]any    `json:"Metadata"`
	StorageJSON     []byte            `json:"StorageJSON"`
	AuthMetadata    map[string]any    `json:"AuthMetadata"`
	AuthAttributes  map[string]string `json:"AuthAttributes"`
	StreamID        string            `json:"stream_id,omitempty"`
	HostCallbackID  string            `json:"host_callback_id,omitempty"`
}

type ExecutorResponse struct {
	Payload  []byte         `json:"Payload"`
	Headers  http.Header    `json:"Headers"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type StreamResponse struct {
	Headers http.Header `json:"Headers"`
}

// 非流式宿主回调直接序列化 pluginapi.HTTPResponse，字段名与流式 RPC 不同。
type upstreamResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type upstreamStream struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type streamChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type Config struct {
	DataDir                   string            `yaml:"data_dir" json:"data_dir"`
	ResponsesURL              string            `yaml:"responses_url" json:"responses_url"`
	UpstreamModel             string            `yaml:"upstream_model" json:"upstream_model"`
	Models                    []string          `yaml:"models" json:"models"`
	ModelMappings             map[string]string `yaml:"model_mappings" json:"model_mappings"`
	TimeoutSeconds            int               `yaml:"timeout_seconds" json:"timeout_seconds"`
	MaxResponseBytes          int               `yaml:"max_response_bytes" json:"max_response_bytes"`
	AuthMode                  string            `yaml:"auth_mode" json:"auth_mode"`
	MaxRequestInlineImages    int               `yaml:"max_request_inline_images" json:"max_request_inline_images"`
	MaxAttachmentCacheEntries int               `yaml:"max_attachment_cache_entries" json:"max_attachment_cache_entries"`
	ToolsVersionID            string            `yaml:"tools_version_id" json:"tools_version_id"`
}

func defaultConfig() Config {
	return Config{
		DataDir:                   "plugins/oai-basispoints-data",
		ResponsesURL:              DefaultResponsesURL,
		UpstreamModel:             DefaultUpstreamModel,
		Models:                    []string{DefaultModelID},
		TimeoutSeconds:            300,
		MaxResponseBytes:          64 << 20,
		MaxRequestInlineImages:    defaultMaxRequestInlineImages,
		MaxAttachmentCacheEntries: defaultAttachmentCacheEntries,
		AuthMode:                  "chatgpt",
	}
}

func (c *Config) normalize() error {
	if c == nil {
		return fail(400, "invalid_config", "configuration is missing")
	}
	c.ResponsesURL = strings.TrimSpace(c.ResponsesURL)
	if c.ResponsesURL == "" {
		c.ResponsesURL = DefaultResponsesURL
	}
	u, err := url.Parse(c.ResponsesURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fail(400, "invalid_config", "responses_url must be an absolute HTTP(S) URL")
	}
	c.UpstreamModel = strings.TrimSpace(c.UpstreamModel)
	if c.UpstreamModel == "" {
		c.UpstreamModel = DefaultUpstreamModel
	}
	c.AuthMode = strings.TrimSpace(c.AuthMode)
	if c.AuthMode == "" {
		c.AuthMode = "chatgpt"
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fail(400, "invalid_config", "timeout_seconds must be between 10 and 1800")
	}
	if c.MaxResponseBytes < 64<<10 || c.MaxResponseBytes > 128<<20 {
		return fail(400, "invalid_config", "max_response_bytes must be between 64 KiB and 128 MiB")
	}
	if c.MaxRequestInlineImages == 0 {
		c.MaxRequestInlineImages = defaultMaxRequestInlineImages
	}
	if c.MaxRequestInlineImages < 1 || c.MaxRequestInlineImages > maxRequestInlineImagesLimit {
		return fail(400, "invalid_config", fmt.Sprintf("max_request_inline_images must be between 1 and %d", maxRequestInlineImagesLimit))
	}
	if c.MaxAttachmentCacheEntries == 0 {
		c.MaxAttachmentCacheEntries = defaultAttachmentCacheEntries
	}
	if c.MaxAttachmentCacheEntries < 1 || c.MaxAttachmentCacheEntries > maxAttachmentCacheEntriesLimit {
		return fail(400, "invalid_config", fmt.Sprintf("max_attachment_cache_entries must be between 1 and %d", maxAttachmentCacheEntriesLimit))
	}
	seen := map[string]bool{}
	models := make([]string, 0, len(c.Models))
	for _, model := range c.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		models = []string{DefaultModelID}
		seen[DefaultModelID] = true
	}
	if c.ModelMappings != nil {
		mappings := make(map[string]string, len(c.ModelMappings))
		for alias, upstream := range c.ModelMappings {
			alias, upstream = strings.TrimSpace(alias), strings.TrimSpace(upstream)
			if alias == "" || upstream == "" {
				return fail(400, "invalid_config", "model_mappings requires non-empty aliases and upstream model names")
			}
			if !seen[alias] {
				return fail(400, "invalid_config", "model_mappings alias is not enabled in models: "+alias)
			}
			if _, exists := mappings[alias]; exists {
				return fail(400, "invalid_config", "model_mappings contains a duplicate normalized alias: "+alias)
			}
			mappings[alias] = upstream
		}
		c.ModelMappings = mappings
	}
	c.Models = models
	return nil
}

func (c Config) clone() Config {
	c.Models = append([]string(nil), c.Models...)
	c.ModelMappings = maps.Clone(c.ModelMappings)
	return c
}

func normalizeEffort(value any) string {
	s, _ := value.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "x-high", "extra-high", "extra_high", "max":
		s = "xhigh"
	}
	if _, ok := supportedReasoningEfforts[s]; ok {
		return s
	}
	return "medium"
}

func rawObject(raw []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fail(400, "invalid_request", "request body must be a JSON object")
	}
	return object, nil
}

func jsonBytes(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func stringValue(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func numberValue(value any) int64 {
	switch n := value.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func errorMessage(body []byte) string {
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		// 校验错误仅保留字段路径和原因，避免把 input 中的私有内容写入日志。
		if details, ok := object["detail"].([]any); ok && len(details) > 0 {
			safe := make([]map[string]any, 0, len(details))
			for _, value := range details {
				entry := objectValue(value)
				if entry == nil {
					continue
				}
				safe = append(safe, map[string]any{"loc": entry["loc"], "msg": entry["msg"], "type": entry["type"]})
			}
			if len(safe) > 0 {
				return string(jsonBytes(map[string]any{"detail": safe}))
			}
		}
		if detail := stringValue(object["detail"]); detail != "" {
			return detail
		}

		if nested, ok := object["error"].(map[string]any); ok {
			if message := stringValue(nested["message"]); message != "" {
				return message
			}
		}
		if message := stringValue(object["message"]); message != "" {
			return message
		}
		if message := stringValue(object["error"]); message != "" {
			return message
		}
	}
	if len(body) > 0 {
		message := strings.TrimSpace(string(body))
		if len(message) > 500 {
			message = message[:500]
		}
		return message
	}
	return "Basis Points upstream request failed"
}

func timeoutError(cfg Config) error {
	return fail(504, "upstream_timeout", fmt.Sprintf("Basis Points request timed out after %d seconds", cfg.TimeoutSeconds))
}

// upstreamModelForAlias 只解析已启用的别名；未单独映射时沿用原有全局配置。
func (c Config) upstreamModelForAlias(alias string) (string, bool) {
	for _, candidate := range c.Models {
		if alias == candidate {
			if upstream, exists := c.ModelMappings[alias]; exists {
				return upstream, true
			}
			return c.UpstreamModel, true
		}
	}
	return "", false
}

// resolveUpstreamModel 同时接受客户端别名和 CPA 执行器传入的已配置上游名称。
func (c Config) resolveUpstreamModel(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" && len(c.Models) > 0 {
		model = c.Models[0]
	}
	if upstream, ok := c.upstreamModelForAlias(model); ok {
		return upstream, true
	}
	for _, alias := range c.Models {
		if upstream, ok := c.upstreamModelForAlias(alias); ok && model == upstream {
			return upstream, true
		}
	}
	return "", false
}
