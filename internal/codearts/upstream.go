// upstream.go CodeArts 上游：签名请求助手 + 模型目录三源（agent/builtin/benefit）
// + 余额两本账（订阅统计 + 福利池）+ 会话心跳。
package codearts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hwauth"
)

const (
	// BaseURL 区域 API 主 base（对话/心跳/统计/目录/活动）。
	BaseURL = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// BenefitGateway 福利网关（另一台机器、另一套签名契约）。
	BenefitGateway = "https://opengw.developer.huaweicloud.com"
	// ChatPath 对话端点。
	ChatPath = "/api/v2/chat/completions"
	// HeartbeatPath 会话心跳。
	HeartbeatPath = "/snap-manager/v1/chat-session/heartbeat"
	// StatisticsPath 订阅统计。
	StatisticsPath = "/snap-manager/v1/statistics/plugin"
	// BenefitBalancePath 福利池余额。
	BenefitBalancePath = "/api/v1/user/tokens/balance"
	// BenefitClaimPath benefit 档案注册（幂等）。
	BenefitClaimPath = "/api/v1/benefit/claim"
	// MaxOutputTokens 上游平台级硬顶（65537 起整条拒收，目录自述不可信）。
	MaxOutputTokens = int64(65536)
)

// Upstream 上游客户端。
type Upstream struct{ HTTP *http.Client }

// NewUpstream 生产默认。
func NewUpstream() *Upstream {
	return &Upstream{HTTP: &http.Client{Timeout: 180 * time.Second}}
}

// signedDo 签名并发出请求，返回 body（调用方负责状态码判定）。
//   - includeHost：福利网关/心跳带 Host，区域 API 不带
//   - domainless：清空 domain_id 再签（不发 X-Domain-Id）
func (u *Upstream) signedDo(method, rawURL string, headers [][2]string, body []byte,
	cred *Credential, includeHost, domainless bool) ([]byte, int, error) {
	sc := cred.Signing()
	if domainless {
		sc.DomainID = ""
	}
	signed, err := hwauth.SignSDK(method, rawURL, headers, body, sc, includeHost)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	if len(body) > 0 {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	for _, kv := range signed.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// 会话心跳（上游每账号 3 路并发；并发闸不认请求结束只关心跳）
// ---------------------------------------------------------------------------

// Heartbeat busy/idle 心跳。签名带 Host、只装 AK/SK/STS（domain 留空 ⇒ 不发
// X-Domain-Id）；成功 = 200 且 body {"status":"ok"}。
func (u *Upstream) Heartbeat(cred *Credential, sessionID, status string) error {
	endpoint := BaseURL + HeartbeatPath + "?status=" + url.QueryEscape(status)
	headers := [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "application/json"},
		{"X-Language", Language},
		{"x-client-type", "kernel"},
		{"user-session-id", sessionID},
		{"x-snap-traceid", uuidV4Hex()},
	}
	hbCred := &Credential{
		AccessKeyID: cred.AccessKeyID, SecretAccess: cred.SecretAccess,
		SecurityTok: cred.SecurityTok,
	}
	raw, code, err := u.signedDo(http.MethodPut, endpoint, headers, []byte("{}"), hbCred, true, false)
	if err != nil {
		return fmt.Errorf("CodeArts 会话心跳传输失败: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("CodeArts 会话心跳被拒（HTTP %d）: %s", code, truncate(string(raw), 200))
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if s, _ := m["status"].(string); s != "ok" {
		return fmt.Errorf("CodeArts 会话心跳未确认 %s 状态: %s", status, truncate(string(raw), 200))
	}
	return nil
}

// uuidV4Hex UUIDv4 去连字符（32 hex）。
func uuidV4Hex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// 模型目录三源合并（agent → builtin → benefit，按 id 先到先得；禁止硬编码兜底）
// ---------------------------------------------------------------------------

// ModelSource 模型来自哪个源（benefit 决定 maas_type 头）。
type ModelSource string

const (
	SourceAgent   ModelSource = "agent"
	SourceBuiltin ModelSource = "builtin"
	SourceBenefit ModelSource = "benefit"
)

// ModelConfig 一条模型（id 是上游真名，大小写敏感）。
type ModelConfig struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	DisplayName     string      `json:"display_name"`
	Description     string      `json:"description,omitempty"`
	ContextLength   int64       `json:"context_length,omitempty"`
	MaxOutputTokens int64       `json:"max_output_tokens,omitempty"`
	SupportsImages  bool        `json:"supports_images,omitempty"`
	Source          ModelSource `json:"source"`
	CreditDisplay   string      `json:"credit_display,omitempty"`
}

// Catalog 合并后的清单。
type Catalog struct {
	Models   []ModelConfig `json:"models"`
	Warnings []string      `json:"warnings,omitempty"`
}

// Resolve 大小写无关查找，返回上游真名条目。
func (c *Catalog) Resolve(requested string) (ModelConfig, bool) {
	wanted := strings.TrimSpace(requested)
	for _, m := range c.Models {
		if strings.EqualFold(m.ID, wanted) {
			return m, true
		}
	}
	return ModelConfig{}, false
}

// catalogHeaders 目录请求公共头；agentType 非空才加 Agent-Type。
func catalogHeaders(agentType string) [][2]string {
	hs := [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "application/json"},
		{"X-Language", Language},
		{"plugin-name", PluginName},
		{"plugin-version", PluginVersion},
		{"client_version", "Vscode_" + PluginVersion},
		{"is_confidential", "false"},
	}
	if agentType != "" {
		hs = append(hs, [][2]string{{"Agent-Type", agentType}}...)
	}
	return hs
}

// fetchJSON 签名 GET + envelope 解析。
func (u *Upstream) fetchJSON(rawURL, agentType string, cred *Credential, includeHost, domainless bool) (map[string]json.RawMessage, error) {
	raw, code, err := u.signedDo(http.MethodGet, rawURL, catalogHeaders(agentType), nil, cred, includeHost, domainless)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", code, truncate(string(raw), 200))
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	return out, nil
}

// Discover 抓三个源并合并。任何一个源失败只记 warning 不中断 —— 目录是
// 「能拿到多少算多少」；全部为空时 warnings 有一条说明。
func (u *Upstream) Discover(cred *Credential) *Catalog {
	catalog := &Catalog{}
	// 源 1：agent（账号实际可用的那批）。
	agentModels, err := u.discoverAgent(cred)
	if err != nil {
		catalog.Warnings = append(catalog.Warnings, "agent 目录: "+err.Error())
	}
	// 源 2：builtin（必须 Agent-Type: PromptCenter；多模态只在这里出现）。
	raw, code, err := u.signedDo(http.MethodGet, BaseURL+"/v1/model/builtin",
		catalogHeaders("PromptCenter"), nil, cred, false, false)
	if err != nil {
		catalog.Warnings = append(catalog.Warnings, "builtin 目录: "+err.Error())
	} else if code != http.StatusOK {
		catalog.Warnings = append(catalog.Warnings, fmt.Sprintf("builtin 目录: HTTP %d", code))
	} else {
		models, perr := parseBuiltin(raw)
		if perr != nil {
			catalog.Warnings = append(catalog.Warnings, "builtin 目录: "+perr.Error())
		}
		agentModels = appendUnique(agentModels, models)
	}
	catalog.Models = agentModels
	// 源 3：benefit（另一台主机、带 Host 且 domainless）。
	benefitModels, err := u.discoverBenefit(cred)
	if err != nil {
		catalog.Warnings = append(catalog.Warnings, "福利目录: "+err.Error())
	} else {
		catalog.Models = appendUnique(catalog.Models, benefitModels)
	}
	if len(catalog.Models) == 0 {
		catalog.Warnings = append(catalog.Warnings, "该账号没有返回任何可用模型")
	}
	return catalog
}

func appendUnique(target, incoming []ModelConfig) []ModelConfig {
	for _, m := range incoming {
		dup := false
		for _, e := range target {
			if e.ID == m.ID {
				dup = true
				break
			}
		}
		if !dup {
			target = append(target, m)
		}
	}
	return target
}

// discoverAgent 分页列 agent → 逐个 detail 取 gpts.models。
func (u *Upstream) discoverAgent(cred *Credential) ([]ModelConfig, error) {
	const page = 100
	var all []ModelConfig
	for p := 0; p < 100; p++ {
		q := url.Values{}
		q.Set("offset", fmt.Sprint(p*page))
		q.Set("limit", fmt.Sprint(page))
		q.Set("is_primary_agent", "true")
		q.Set("supported_client", "VSCODE")
		q.Set("min_compatible_plugin_version", PluginVersion)
		rawURL := BaseURL + "/v1/agent-center/agents/useragents?" + q.Encode()
		body, err := u.fetchJSON(rawURL, "AgentCenter", cred, false, false)
		if err != nil {
			if p == 0 {
				return nil, err
			}
			break
		}
		var ids []string
		if agentsRaw, ok := body["agents"]; ok {
			var agents []struct {
				AgentID    string `json:"agent_id"`
				OriginalID string `json:"original_id"`
			}
			if json.Unmarshal(agentsRaw, &agents) == nil {
				for _, a := range agents {
					id := a.AgentID
					if id == "" {
						id = a.OriginalID
					}
					if id != "" {
						ids = append(ids, id)
					}
				}
			}
		}
		for _, id := range ids {
			detailURL := BaseURL + "/v1/agent-center/agents/detail?agent_id=" + url.QueryEscape(id)
			dbody, derr := u.fetchJSON(detailURL, "AgentCenter", cred, false, false)
			if derr != nil {
				continue
			}
			models, perr := parseAgentDetail(dbody)
			if perr != nil {
				continue
			}
			all = appendUnique(all, models)
		}
		if len(ids) < page {
			break
		}
	}
	return all, nil
}

// parseAgentDetail gpts.models → 清单。enabled 显式 false 跳过；
// display_enabled 缺失也算不可见（trial 账号实测如此）。
func parseAgentDetail(body map[string]json.RawMessage) ([]ModelConfig, error) {
	gptsRaw, ok := body["gpts"]
	if !ok {
		return nil, fmt.Errorf("agent detail 没有 gpts 目录（该账号可能没有可用模型）")
	}
	var gpts struct {
		Models []struct {
			ModelAlias string `json:"model_alias"`
			ModelName  string `json:"model_name"`
			Parameters struct {
				ModelID       string `json:"model_id"`
				ContextWindow int64  `json:"context_window"`
				MaxTokens     int64  `json:"max_tokens"`
				SupportsImg   bool   `json:"supports_images"`
				DisplayEnable *bool  `json:"display_enabled"`
				Enabled       *bool  `json:"enabled"`
				Desc          string `json:"model_desc"`
				DescEn        string `json:"model_desc_en"`
			} `json:"model_parameters"`
			Credit []struct {
				RatioDisplay string `json:"ratio_display"`
				Status       string `json:"status"`
				InputFrom    int64  `json:"input_from"`
			} `json:"credit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(gptsRaw, &gpts); err != nil {
		return nil, err
	}
	var out []ModelConfig
	for _, m := range gpts.Models {
		if m.Parameters.Enabled != nil && !*m.Parameters.Enabled {
			continue
		}
		if m.Parameters.DisplayEnable == nil || !*m.Parameters.DisplayEnable {
			continue
		}
		id := m.Parameters.ModelID
		if id == "" {
			id = m.ModelAlias
		}
		if id == "" {
			continue
		}
		name := m.ModelName
		if name == "" {
			name = id
		}
		out = append(out, ModelConfig{
			ID: id, Name: name, DisplayName: name,
			Description:     firstNonEmpty(m.Parameters.Desc, m.Parameters.DescEn),
			ContextLength:   m.Parameters.ContextWindow,
			MaxOutputTokens: cappedOutput(m.Parameters.MaxTokens),
			SupportsImages:  m.Parameters.SupportsImg,
			Source:          SourceAgent,
			CreditDisplay:   creditDisplayOf(len(m.Credit), func(i int) (string, int64) { return m.Credit[i].RatioDisplay, m.Credit[i].InputFrom }, len(m.Credit)),
		})
	}
	return out, nil
}

// creditDisplayOf 取 credit[] 中 input_from==0 的基础档 ratio_display；
// status 显式非 "0" 跳过、缺 status 当生效；无基础档退第一条可读档。
func creditDisplayOf(n int, at func(i int) (string, int64), _ int) string {
	fallback := ""
	for i := 0; i < n; i++ {
		ratio, from := at(i)
		if from != 0 {
			continue
		}
		if ratio != "" {
			return ratio
		}
	}
	for i := 0; i < n; i++ {
		ratio, _ := at(i)
		if ratio != "" && fallback == "" {
			fallback = ratio
		}
	}
	return fallback
}

func cappedOutput(v int64) int64 {
	if v > MaxOutputTokens {
		return MaxOutputTokens
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseBuiltin /v1/model/builtin：enable 显式 false 才跳过（缺字段当可用）。
func parseBuiltin(raw []byte) ([]ModelConfig, error) {
	var payload struct {
		BuiltinModels []struct {
			ModelID      string `json:"model_id"`
			ModelName    string `json:"model_name"`
			Enable       *bool  `json:"enable"`
			Desc         string `json:"model_desc"`
			ContextWin   int64  `json:"context_window"`
			MaxTokens    int64  `json:"max_tokens"`
			SupportsImg  bool   `json:"supports_images"`
			Credit       []struct {
				RatioDisplay string `json:"ratio_display"`
			} `json:"credit"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("builtin 目录不是合法 JSON: %w", err)
	}
	var out []ModelConfig
	for _, m := range payload.BuiltinModels {
		if m.Enable != nil && !*m.Enable {
			continue
		}
		id := m.ModelID
		if id == "" {
			id = m.ModelName
		}
		if id == "" {
			continue
		}
		name := m.ModelName
		if name == "" {
			name = id
		}
		cd := ""
		if len(m.Credit) > 0 {
			cd = m.Credit[0].RatioDisplay
		}
		out = append(out, ModelConfig{
			ID: id, Name: name, DisplayName: name,
			Description:     m.Desc,
			ContextLength:   m.ContextWin,
			MaxOutputTokens: cappedOutput(m.MaxTokens),
			SupportsImages:  m.SupportsImg,
			Source:          SourceBuiltin,
			CreditDisplay:   cd,
		})
	}
	return out, nil
}

// discoverBenefit 先看总开关，开了才去福利网关取目录（签名带 Host、domainless）。
func (u *Upstream) discoverBenefit(cred *Credential) ([]ModelConfig, error) {
	gate, err := u.fetchJSON(BaseURL+"/v1/benefit-gateway-config", "PromptCenter", cred, false, false)
	if err != nil {
		return nil, err
	}
	var gateBody struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(needRaw(gate), &gateBody); err == nil {
		if gateBody.Enabled == nil || !*gateBody.Enabled {
			return nil, nil
		}
	}
	raw, code, err := u.signedDo(http.MethodGet, BenefitGateway+"/api/v1/gateway/config",
		catalogHeaders(""), nil, cred, true, true)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	return parseBenefit(raw)
}

// needRaw 把 envelope 重编码成原始字节（gate 响应整体）。
func needRaw(m map[string]json.RawMessage) []byte {
	raw, _ := json.Marshal(m)
	return raw
}

// parseBenefit 福利目录：成功看 envelope 的 error_code=="0000"（不看 HTTP 状态）。
func parseBenefit(raw []byte) ([]ModelConfig, error) {
	var payload struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			Models []struct {
				ModelID      string `json:"model_id"`
				ModelName    string `json:"model_name"`
				ContextWin   int64  `json:"context_window"`
				MaxTokens    int64  `json:"max_tokens"`
				Sort         int64  `json:"sort"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("福利目录不是合法 JSON: %w", err)
	}
	if payload.ErrorCode != "0000" {
		return nil, fmt.Errorf("福利目录未成功（error_code=%s）", payload.ErrorCode)
	}
	// 按 sort 稳定升序。
	models := payload.Result.Models
	for i := 1; i < len(models); i++ {
		for j := i; j > 0 && models[j].Sort < models[j-1].Sort; j-- {
			models[j], models[j-1] = models[j-1], models[j]
		}
	}
	var out []ModelConfig
	for _, m := range models {
		if m.ModelID == "" {
			continue
		}
		name := m.ModelName
		if name == "" {
			name = m.ModelID
		}
		out = append(out, ModelConfig{
			ID: m.ModelID, Name: name, DisplayName: name,
			ContextLength:   m.ContextWin,
			MaxOutputTokens: cappedOutput(m.MaxTokens),
			Source:          SourceBenefit,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("福利网关没有返回任何模型")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 余额两本账（订阅统计 + 福利池，两份互相不知道对方存在的账）
// ---------------------------------------------------------------------------

// Meter 订阅统计一行。
type Meter struct {
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	UsedTokens   int64    `json:"used_tokens,omitempty"`
	AllowTokens  int64    `json:"allowance_tokens,omitempty"`
	CreditTotal  *float64 `json:"credit_total,omitempty"`
	CreditUsed   *float64 `json:"credit_used,omitempty"`
	CreditRemain *float64 `json:"credit_remaining,omitempty"`
	UsedPercent  *float64 `json:"used_percent,omitempty"`
}

// Statistics 订阅统计文档。
type Statistics struct {
	ResetDate string  `json:"reset_date,omitempty"`
	Plan      string  `json:"plan,omitempty"`
	PlanName  string  `json:"plan_name,omitempty"`
	Meters    []Meter `json:"meters"`
}

// BenefitBalance 福利池余额。
type BenefitBalance struct {
	Channel           string `json:"channel,omitempty"`
	DailyTokenLimit   int64  `json:"daily_token_limit"`
	DailyTokensUsed   int64  `json:"daily_tokens_used"`
	MonthlyTokenLimit int64  `json:"monthly_token_limit"`
	MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
	TotalBalance      int64  `json:"total_balance"`
}

// FetchStatistics 订阅统计（付费套餐计量表；头集合固定四件套，签名不带 Host）。
func (u *Upstream) FetchStatistics(cred *Credential) (*Statistics, error) {
	headers := [][2]string{
		{"Accept", "application/json"},
		{"X-Language", Language},
		{"plugin-name", PluginName},
		{"plugin-version", PluginVersion},
	}
	raw, code, err := u.signedDo(http.MethodGet, BaseURL+StatisticsPath, headers, nil, cred, false, false)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("统计接口返回 HTTP %d: %s", code, truncate(string(raw), 200))
	}
	var payload struct {
		EndDate string `json:"end_date"`
		Metrics []struct {
			Name         string   `json:"name"`
			Value        *float64 `json:"value"`
			UsedTokens   int64    `json:"usage_token_num"`
			AllowTokens  int64    `json:"package_token_amount"`
			CreditTotal  *float64 `json:"package_credit_amount"`
			CreditUsed   *float64 `json:"package_credit_used"`
			CreditRemain *float64 `json:"package_credit_remain"`
			Show         bool     `json:"show"`
		} `json:"metrics"`
		Package struct {
			SpecCode   string `json:"spec_code"`
			NameEn     string `json:"package_name_en"`
			NameCn     string `json:"package_name_cn"`
		} `json:"package"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("统计响应不是合法 JSON: %w", err)
	}
	out := &Statistics{ResetDate: payload.EndDate,
		Plan: payload.Package.SpecCode,
		PlanName: firstNonEmpty(payload.Package.NameEn, payload.Package.NameCn)}
	for _, m := range payload.Metrics {
		if m.Name == "" || !m.Show {
			continue
		}
		row := Meter{Name: m.Name, Label: meterLabel(m.Name), UsedTokens: m.UsedTokens,
			AllowTokens: m.AllowTokens, CreditTotal: m.CreditTotal,
			CreditUsed: m.CreditUsed, CreditRemain: m.CreditRemain}
		if m.Value != nil && *m.Value >= 0 {
			row.UsedPercent = m.Value
		}
		out.Meters = append(out.Meters, row)
	}
	return out, nil
}

// meterLabel 计量表中文标签（照抄 agent2api quota.go 的表；认不出回落原名）。
func meterLabel(name string) string {
	switch name {
	case "usageDataCodeCompletions":
		return "代码补全额度"
	case "usageDataChatMessages":
		return "对话消息额度"
	case "usageTokenChatMessages":
		return "对话 token 额度"
	case "usageTotalPackageCredit":
		return "套餐积分"
	case "usageBasicPackageCredit":
		return "基础包积分"
	case "usageOnDemandPackageCredit":
		return "按需付费积分"
	case "usageBonusPackageCredit":
		return "赠送积分"
	}
	return name
}

// FetchBenefitBalance 福利池余额（头同统计四件套；签名带 Host、domainless）。
// 返回 (nil, nil) = 上游明说「该账号没有福利数据」（4004 + benefit not found）。
func (u *Upstream) FetchBenefitBalance(cred *Credential) (*BenefitBalance, error) {
	headers := [][2]string{
		{"Accept", "application/json"},
		{"X-Language", Language},
		{"plugin-name", PluginName},
		{"plugin-version", PluginVersion},
	}
	raw, code, err := u.signedDo(http.MethodGet, BenefitGateway+BenefitBalancePath, headers, nil, cred, true, true)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("福利余额返回 HTTP %d", code)
	}
	return parseBenefitBalance(raw)
}

func parseBenefitBalance(raw []byte) (*BenefitBalance, error) {
	var payload struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    *struct {
			Channel           string `json:"channel"`
			DailyTokenLimit   int64  `json:"daily_token_limit"`
			DailyTokensUsed   int64  `json:"daily_tokens_used"`
			MonthlyTokenLimit int64  `json:"monthly_token_limit"`
			MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
			TotalBalance      int64  `json:"total_balance"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("福利余额响应不是合法 JSON: %w", err)
	}
	if payload.ErrorCode != "0000" {
		if payload.ErrorCode == "4004" && strings.Contains(strings.ToLower(payload.ErrorMsg), "benefit not found") {
			return nil, nil // 正常状态：该账号没有福利池
		}
		return nil, fmt.Errorf("福利余额返回 %s: %s", payload.ErrorCode, truncate(payload.ErrorMsg, 200))
	}
	if payload.Result == nil {
		return nil, fmt.Errorf("福利余额响应没有 result")
	}
	return &BenefitBalance{
		Channel: payload.Result.Channel,
		DailyTokenLimit: payload.Result.DailyTokenLimit,
		DailyTokensUsed: payload.Result.DailyTokensUsed,
		MonthlyTokenLimit: payload.Result.MonthlyTokenLimit,
		MonthlyTokensUsed: payload.Result.MonthlyTokensUsed,
		TotalBalance: payload.Result.TotalBalance,
	}, nil
}

// ClaimBenefit benefit 档案注册（幂等；官方 initBenefit 同款）。没有档案的账号
// 上游会以 4004 benefit not found 拒答。
func (u *Upstream) ClaimBenefit(cred *Credential) (string, error) {
	headers := [][2]string{
		{"Agent-Type", "PromptCenter"},
		{"Content-Type", "application/json"},
		{"X-Language", Language},
	}
	if cred.SecurityTok != "" {
		headers = append([][2]string{{"X-Security-Token", cred.SecurityTok}}, headers...)
	}
	raw, code, err := u.signedDo(http.MethodPost, BenefitGateway+BenefitClaimPath, headers, []byte("{}"), cred, true, true)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("benefit 领取返回 HTTP %d", code)
	}
	var payload struct {
		ErrorCode string          `json:"error_code"`
		ErrorMsg  string          `json:"error_msg"`
		Result    json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("benefit 领取响应不是合法 JSON: %w", err)
	}
	if payload.ErrorCode != "0000" {
		return "", fmt.Errorf("benefit 领取返回 %s: %s", payload.ErrorCode, truncate(payload.ErrorMsg, 200))
	}
	var s string
	if json.Unmarshal(payload.Result, &s) == nil && s != "" {
		return s, nil
	}
	var obj map[string]any
	if json.Unmarshal(payload.Result, &obj) == nil {
		if m, _ := obj["msg"].(string); m != "" {
			return m, nil
		}
		if m, _ := obj["message"].(string); m != "" {
			return m, nil
		}
	}
	return "官方已确认 benefit 档案", nil
}
