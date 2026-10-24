// upstream.go OfficeAce 上游：chat（Basic auth，恒流式）+ 订阅/倍率/签到（V11 签名）
// + 模型目录（OpenAI /v2/models）+ max_tokens=1 模型探测。
package officeace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hwauth"
)

const (
	// ReasoningAllowance 上游把 max_tokens 当「推理+正文」总预算：给小了会 200 但
	// 正文空串，追加（不是 max）1024；< 16 原样透传。
	ReasoningAllowance = 1024
	// TinyMaxTokens 小于该值的 max_tokens 原样透传。
	TinyMaxTokens = 64
	// CatalogTTL 模型目录自动刷新 TTL。
	CatalogTTL = 10 * time.Minute
	// ProbeTTL 模型可用性探测 TTL。
	ProbeTTL = 6 * time.Hour
	// ProbeConcurrency 探测并发。
	ProbeConcurrency = 4
)

// Upstream 上游客户端。
type Upstream struct{ HTTP *http.Client }

// NewUpstream 生产默认。
func NewUpstream() *Upstream {
	return &Upstream{HTTP: &http.Client{Timeout: 180 * time.Second}}
}

// basicAuth Basic base64(key:secret)。
func basicAuth(key, secret string) string {
	return "Basic " + base64Std([]byte(key+":"+secret))
}

// ---------------------------------------------------------------------------
// chat（无状态 OpenAI 兼容；恒流式）
// ---------------------------------------------------------------------------

// PrepareChatBody 请求体改写：强制写上游真名（客户端体里的 "officeace:" 前缀名
// 必须剥掉——上游只认裸名，带前缀整条 81009 Invalid model）、强制 stream:true、
// max_tokens 追加预算、补 include_usage。
func PrepareChatBody(body map[string]any, model string) {
	if model != "" {
		body["model"] = model
	}
	body["stream"] = true
	if n, ok := body["max_tokens"].(float64); ok && n >= TinyMaxTokens {
		body["max_tokens"] = n + ReasoningAllowance
	}
	if so, ok := body["stream_options"].(map[string]any); ok {
		if _, has := so["include_usage"]; !has {
			so["include_usage"] = true
		}
	} else {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
}

// ChatForward 发一次对话（恒流式），返回原始 SSE 流。
func (u *Upstream) ChatForward(cred *Credential, payload []byte) (io.ReadCloser, error) {
	endpoint := ChatEndpoint(cred.BaseURL)
	if endpoint == "" {
		return nil, fmt.Errorf("OfficeAce 账号缺少模型网关基址")
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", basicAuth(cred.ModelAppKey, cred.ModelAppSecret))
	req.Header.Set("Chat-Id", randomHex(16))
	req.Header.Set("Session-Id", randomHex(16))
	req.Header.Set("lang", "en")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OfficeAce 对话传输失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	return resp.Body, nil
}

// UpstreamError 上游非 2xx（带原始状态码与体，供分类）。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("上游返回 %d: %s", e.Status, truncate(strings.TrimSpace(e.Body), 300))
}

// Classify 错误分类（对齐 agent2api adapter.rs classify_error 顺序）：
// 1) 体积超限标记 → 413 Fatal；2) 429/限流标记 → 可冷却换号；3) 其余 Fatal。
func (e *UpstreamError) Classify() (status int, cooldownable bool) {
	lowered := strings.ToLower(e.Body)
	sizeMarkers := []string{"81113", "exceeds the maximum size", "requestentitytoolarge",
		"payload too large", "request body is too large", "request body is too long"}
	for _, m := range sizeMarkers {
		if strings.Contains(lowered, m) {
			return 413, false
		}
	}
	rateMarkers := []string{"81111", "81112", "81114", "0308", "tpm", "rate limit", "too many requests"}
	if e.Status == 429 {
		return 429, true
	}
	for _, m := range rateMarkers {
		if strings.Contains(lowered, m) {
			return 429, true
		}
	}
	return e.Status, false
}

// ContentFilter 判流内容审核拒绝（finish_reason=content_filter 的假答复）。
func ContentFilter(ssePayload []byte) bool {
	return strings.Contains(strings.ToLower(string(ssePayload)), "content_filter")
}

// ---------------------------------------------------------------------------
// 控制面（V11 签名；网关根 = 剥 /v2）
// ---------------------------------------------------------------------------

// gatewayRoot 剥尾 /v2（订阅族挂在网关根）。
func gatewayRoot(baseURL string) string {
	b := normalizeBase(baseURL)
	return strings.TrimSuffix(b, "/v2")
}

// controlCall V11 签名控制面请求。
func (u *Upstream) controlCall(cred *Credential, method, path string, body []byte, extraHeaders [][2]string) (map[string]json.RawMessage, error) {
	if !cred.HasControlPlane() {
		return nil, fmt.Errorf("OfficeAce 账号没有控制面凭据（导入时只给了网关 Basic 那一对）")
	}
	rawURL := gatewayRoot(cred.BaseURL) + path
	headers := append([][2]string{}, extraHeaders...)
	if len(body) > 0 {
		headers = append(headers, [][2]string{{"Content-Type", "application/json"}}...)
	}
	signed, err := hwauth.SignV11(method, rawURL, headers, body, cred.Signing(), Region)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	for _, kv := range signed.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OfficeAce 控制面请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("OfficeAce 控制面凭据已失效（临时 AK/SK 会过期，HTTP %d）：请重新登录或刷新", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OfficeAce 控制面返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 240))
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("OfficeAce 控制面响应不是合法 JSON: %w", err)
	}
	return out, nil
}

// num 上游数值字段是字符串（"500"）——数字/字符串双形态解析。
func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

// numOpt 缺失/空串/非数字 → nil（明细行「上游没给」与「真的 0」不混）。
func numOpt(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case int64:
		f := float64(t)
		return &f
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
			return &f
		}
	}
	return nil
}

// Subscription 订阅快照解析结果。
type Subscription struct {
	Available float64 `json:"available"`
	Unit      string  `json:"unit"`
	Wallets   []Wallet `json:"wallets"`
	PlanName  string  `json:"planName"`
	Status    string  `json:"status"`
	ExpireAt  int64   `json:"expireAt,omitempty"`
	RemainQuota float64 `json:"remainQuota"`
	TotalQuota  float64 `json:"totalQuota"`
	Credits   []CreditRow `json:"credits,omitempty"`
}

// Wallet 钱包行。
type Wallet struct {
	Type        string  `json:"type"`
	DisplayName string  `json:"displayName"`
	Balance     float64 `json:"balance"`
}

// CreditRow 明细行（缺失的键整个不出）。
type CreditRow struct {
	Name     string   `json:"name"`
	Total    *float64 `json:"total,omitempty"`
	Used     *float64 `json:"used,omitempty"`
	Remain   *float64 `json:"remain,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	ExpireAt int64    `json:"expireAt,omitempty"`
}

// wholePoints 向下取整（积分是整数概念；宁可少报零头不虚报）。
func wholePoints(v float64) float64 {
	if v != v || v > 1e15 || v < -1e15 || v <= 0 {
		return 0
	}
	return float64(int64(v))
}

// FetchSubscription GET /v1/subscription（x-subscription-type: v2）→ summarize。
func (u *Upstream) FetchSubscription(cred *Credential) (*Subscription, error) {
	out, err := u.controlCall(cred, http.MethodGet, "/v1/subscription", nil,
		[][2]string{{"x-subscription-type", "v2"}})
	if err != nil {
		return nil, err
	}
	return summarize(out), nil
}

// summarize 把 /v1/subscription 归一（只算未过期额度；available = 套餐 + 奖励）。
func summarize(raw map[string]json.RawMessage) *Subscription {
	var doc struct {
		SubscribeStatus string `json:"subscribe_status"`
		SKUs []struct {
			SKUName    string `json:"sku_name"`
			ExpireTime string `json:"expired_time"`
			Quotas     []struct {
				AttrCode    string `json:"sku_attr_code"`
				SKUValue    any    `json:"sku_value"`
				CurrentValue any  `json:"current_value"`
				ExpireTime  string `json:"expired_time"`
			} `json:"quotas"`
		} `json:"skus"`
		BonusSKUs []struct {
			ActivityID   string `json:"activity_id"`
			CBCResourceID string `json:"cbc_resource_id"`
			ActivityName string `json:"activity_name"`
			Points       any    `json:"points"`
			CurrentValue any  `json:"current_value"`
			ExpireTime   string `json:"expired_time"`
		} `json:"bonus_skus"`
	}
	_ = json.Unmarshal(mustJSON(raw), &doc)
	now := time.Now().UnixMilli()
	sub := &Subscription{Unit: "积分", Status: doc.SubscribeStatus}
	var planTotal, planUsed, bonusTotal, bonusUsed, searchTotal, searchUsed float64
	var rows []CreditRow
	planExpire := int64(0)
	for _, sku := range doc.SKUs {
		if sub.PlanName == "" {
			sub.PlanName = sku.SKUName
		}
		if ms, ok := parseRFC3339Ms(sku.ExpireTime); ok && ms > 0 && (planExpire == 0 || ms < planExpire) {
			planExpire = ms
		}
		for _, q := range sku.Quotas {
			expired, _ := parseRFC3339Ms(q.ExpireTime)
			if expired > 0 && expired <= now {
				continue
			}
			name := sku.SKUName
			if name == "" {
				name = "套餐积分"
			}
			switch q.AttrCode {
			case "officeace_points":
				planTotal += num(q.SKUValue)
				planUsed += num(q.CurrentValue)
				total, used := numOpt(q.SKUValue), numOpt(q.CurrentValue)
				var remain *float64
				if total != nil && used != nil {
					r := wholePoints(*total - *used)
					remain = &r
				}
				rows = append(rows, CreditRow{Name: name, Total: total, Used: used, Remain: remain, ExpireAt: expired})
			case "online_search_count":
				searchTotal += num(q.SKUValue)
				searchUsed += num(q.CurrentValue)
			}
		}
	}
	type bonus struct{ id, name string; total, used float64; expires int64 }
	var bonuses []bonus
	for _, b := range doc.BonusSKUs {
		key := b.ActivityID
		if key == "" {
			key = b.CBCResourceID
		}
		if key == "" {
			key = b.ActivityName
		}
		if key == "" {
			continue
		}
		name := b.ActivityName
		if name == "" {
			name = key
		}
		expired, _ := parseRFC3339Ms(b.ExpireTime)
		bonuses = append(bonuses, bonus{id: key, name: name,
			total: num(b.Points), used: num(b.CurrentValue), expires: expired})
	}
	for _, b := range bonuses {
		if b.expires == 0 || b.expires > now {
			bonusTotal += b.total
			bonusUsed += b.used
		}
		rows = append(rows, CreditRow{Name: b.name,
			Total: ptr(b.total), Used: ptr(b.used), Remain: ptr(wholePoints(b.total - b.used)),
			ExpireAt: b.expires})
	}
	sub.RemainQuota = wholePoints(planTotal - planUsed)
	sub.TotalQuota = planTotal
	bonusRemaining := wholePoints(bonusTotal - bonusUsed)
	sub.Available = sub.RemainQuota + bonusRemaining
	sub.ExpireAt = planExpire
	sub.Wallets = []Wallet{
		{Type: "plan", DisplayName: fallbackName(sub.PlanName), Balance: sub.RemainQuota},
		{Type: "bonus", DisplayName: "奖励积分", Balance: bonusRemaining},
	}
	if searchTotal > 0 {
		rows = append(rows, CreditRow{Name: "联网搜索",
			Total: ptr(searchTotal), Used: ptr(searchUsed), Remain: ptr(wholePoints(searchTotal - searchUsed)),
			Unit: "次"})
	}
	if len(rows) > 0 {
		// 最近到期在前（无到期排最后），同级按名字。
		for i := 1; i < len(rows); i++ {
			for j := i; j > 0 && rowLess(rows[j], rows[j-1]); j-- {
				rows[j], rows[j-1] = rows[j-1], rows[j]
			}
		}
		sub.Credits = rows
	}
	return sub
}

func rowLess(a, b CreditRow) bool {
	ka, kb := a.ExpireAt, b.ExpireAt
	if ka == 0 {
		ka = int64(1) << 62
	}
	if kb == 0 {
		kb = int64(1) << 62
	}
	if ka != kb {
		return ka < kb
	}
	return a.Name < b.Name
}

func ptr(f float64) *float64 { return &f }

func fallbackName(n string) string {
	if n == "" {
		return "OfficeAce 果办"
	}
	return n
}

// Rates 每模型倍率（model_name 小写 → cost_coefficient）。
type Rates map[string]float64

// FetchRates GET /v1/model-token-rates。空读数不抹掉上一份（防 STS 过期导致整列变假空）。
func (u *Upstream) FetchRates(cred *Credential) (Rates, error) {
	out, err := u.controlCall(cred, http.MethodGet, "/v1/model-token-rates", nil, nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []struct {
			ModelName string `json:"model_name"`
			Cost      any    `json:"cost_coefficient"`
		} `json:"models"`
	}
	if err := json.Unmarshal(mustJSON(out), &doc); err != nil {
		return nil, err
	}
	rates := Rates{}
	for _, m := range doc.Models {
		name := strings.ToLower(strings.TrimSpace(m.ModelName))
		if name == "" {
			continue
		}
		v := numOpt(m.Cost)
		if v == nil || *v < 0 {
			continue
		}
		rates[name] = *v
	}
	return rates, nil
}

// ClaimBonus POST /v1/subscription/bonus/claim（body {}，幂等；签到与新手任务同动作）。
func (u *Upstream) ClaimBonus(cred *Credential) (*Subscription, error) {
	if _, err := u.controlCall(cred, http.MethodPost, "/v1/subscription/bonus/claim", []byte("{}"),
		[][2]string{{"x-subscription-type", "v2"}}); err != nil {
		return nil, err
	}
	// 重复领不报错但当天返回的 bonus_skus 可能不完整：claim 后重查一次订阅拿全量账本。
	return u.FetchSubscription(cred)
}

// ---------------------------------------------------------------------------
// 模型目录（GET {base}/v2/models，标准 OpenAI 或裸数组；只要 Basic）
// ---------------------------------------------------------------------------

// CatalogEntry 目录条目。
type CatalogEntry struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
	OwnedBy       string `json:"owned_by,omitempty"`
	Credits       string `json:"credits,omitempty"`
}

// catalogState 目录缓存。
type catalogState struct {
	mu      sync.Mutex
	entries []CatalogEntry
	fetched time.Time
}

var catalog = &catalogState{}

// FetchModels 拉模型目录（TTL 内走缓存；force 绕过）。
func (u *Upstream) FetchModels(cred *Credential, force bool) ([]CatalogEntry, error) {
	catalog.mu.Lock()
	if !force && len(catalog.entries) > 0 && time.Since(catalog.fetched) < CatalogTTL {
		out := catalog.entries
		catalog.mu.Unlock()
		return out, nil
	}
	catalog.mu.Unlock()
	endpoint := normalizeBase(cred.BaseURL) + "/models"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", basicAuth(cred.ModelAppKey, cred.ModelAppSecret))
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var generic struct {
		Data []struct {
			ID           string `json:"id"`
			DisplayName  string `json:"display_name"`
			ContextWin   int64  `json:"context_window"`
			OwnedBy      string `json:"owned_by"`
		} `json:"data"`
	}
	entries := []CatalogEntry{}
	if json.Unmarshal(raw, &generic) == nil && len(generic.Data) > 0 {
		for _, m := range generic.Data {
			if m.ID == "" {
				continue
			}
			e := CatalogEntry{ID: m.ID, DisplayName: m.ID, ContextLength: m.ContextWin, OwnedBy: m.OwnedBy}
			if m.DisplayName != "" {
				e.DisplayName = m.DisplayName
			}
			entries = append(entries, e)
		}
	} else {
		// 裸数组形态。
		var arr []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			ContextWin  int64  `json:"context_window"`
			OwnedBy     string `json:"owned_by"`
		}
		if json.Unmarshal(raw, &arr) != nil {
			return nil, fmt.Errorf("OfficeAce 模型目录为空或格式不认识（保留现有清单）")
		}
		for _, m := range arr {
			if m.ID == "" {
				continue
			}
			e := CatalogEntry{ID: m.ID, DisplayName: m.ID, ContextLength: m.ContextWin, OwnedBy: m.OwnedBy}
			if m.DisplayName != "" {
				e.DisplayName = m.DisplayName
			}
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("OfficeAce 模型目录为空或格式不认识（保留现有清单）")
	}
	catalog.mu.Lock()
	catalog.entries = entries
	catalog.fetched = time.Now()
	catalog.mu.Unlock()
	return entries, nil
}

// CachedModels 当前缓存清单（未拉过 nil）。
func CachedModels() []CatalogEntry {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return catalog.entries
}

// AttachRates 倍率并进目录条目（只按精确 id 匹配，小写；不做前缀借用——错一个
// 数量级比空着坏得多）。空读数不抹已有读数。
var lastRates Rates

// creditsText 倍率数字 → 界面文本（x1 credits / x0.25 credits）。
func creditsText(rate float64) string {
	rounded := float64(int64(rate*100+0.5)) / 100
	if rounded == float64(int64(rounded)) {
		return fmt.Sprintf("x%d credits", int64(rounded))
	}
	s := fmt.Sprintf("%.2f", rounded)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return "x" + s + " credits"
}

// AttachRates 把倍率写入条目副本。
func AttachRates(entries []CatalogEntry, rates Rates) []CatalogEntry {
	if len(rates) == 0 {
		return entries
	}
	lastRates = rates
	out := make([]CatalogEntry, len(entries))
	copy(out, entries)
	for i := range out {
		id := strings.ToLower(strings.TrimSpace(out[i].ID))
		if v, ok := rates[id]; ok {
			out[i].Credits = creditsText(v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 模型可用性探测（目录 28~34 个、一个号只有约 11 个真能用；probe 找出打不通的）
// ---------------------------------------------------------------------------

var probeState struct {
	mu       sync.Mutex
	hidden   map[string]bool
	probedAt time.Time
	account  string
	probing  bool
}

// HiddenIDs 当前应隐藏的模型（没探过 = 空 = 一个不藏，宁漏藏不错藏）。
func HiddenIDs() []string {
	probeState.mu.Lock()
	defer probeState.mu.Unlock()
	out := make([]string, 0, len(probeState.hidden))
	for id := range probeState.hidden {
		out = append(out, id)
	}
	return out
}

// IsHidden 大小写不敏感匹配。
func IsHidden(id string) bool {
	probeState.mu.Lock()
	defer probeState.mu.Unlock()
	return probeState.hidden[strings.ToLower(strings.TrimSpace(id))]
}

// MaybeProbe 目录刷新成功后调用：到了该重探的时候起一轮后台探测。
func (u *Upstream) MaybeProbe(cred *Credential, entries []CatalogEntry, force bool) {
	if !cred.CanForward() || len(entries) == 0 {
		return
	}
	probeState.mu.Lock()
	// 先丢掉不在当前目录里的旧结论。
	pruned := map[string]bool{}
	for id := range probeState.hidden {
		for _, e := range entries {
			if strings.EqualFold(e.ID, id) {
				pruned[id] = true
				break
			}
		}
	}
	probeState.hidden = pruned
	if probeState.probing {
		probeState.mu.Unlock()
		return
	}
	if !force && !probeState.probedAt.IsZero() && probeState.account == cred.SafeUID() &&
		time.Since(probeState.probedAt) < ProbeTTL {
		probeState.mu.Unlock()
		return
	}
	probeState.probing = true
	probeState.mu.Unlock()

	go func() {
		defer func() {
			probeState.mu.Lock()
			probeState.probing = false
			probeState.mu.Unlock()
		}()
		hidden := u.runProbe(cred, entries)
		probeState.mu.Lock()
		probeState.hidden = hidden
		probeState.probedAt = time.Now()
		probeState.account = cred.SafeUID()
		probeState.mu.Unlock()
	}()
}

// runProbe 对每个模型发 max_tokens=1 探测（只要「上游认不认这个名字」一个 bit）。
func (u *Upstream) runProbe(cred *Credential, entries []CatalogEntry) map[string]bool {
	endpoint := ChatEndpoint(cred.BaseURL)
	if endpoint == "" {
		return nil
	}
	auth := basicAuth(cred.ModelAppKey, cred.ModelAppSecret)
	type result struct {
		id       string
		rejected bool
	}
	sem := make(chan struct{}, ProbeConcurrency)
	results := make([]result, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = result{id: id, rejected: u.probeOne(endpoint, auth, id)}
		}(i, e.ID)
	}
	wg.Wait()
	hidden := map[string]bool{}
	rejectedCount := 0
	for _, r := range results {
		if r.rejected {
			hidden[strings.ToLower(r.id)] = true
			rejectedCount++
		}
	}
	// 整轮全拒 = 凭据/网关问题，一个都不隐藏。
	if len(entries) > 0 && rejectedCount == len(entries) {
		return nil
	}
	return hidden
}

// probeOne 探一个模型。隐藏判据：响应体含明确模型级拒绝标记；
// HTTP 401 或响应体带 apig. = 凭据级失败不算模型拒绝。
func (u *Upstream) probeOne(endpoint, auth, model string) bool {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, model)
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false // 传输/超时拿不到结论 ⇒ 不藏
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false
	}
	lowered := strings.ToLower(string(raw))
	if resp.StatusCode == 401 || strings.Contains(lowered, "apig.") {
		return false
	}
	for _, marker := range []string{"81004", "81009", "81010", "81016", "insufficient permission", "invalid model"} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func base64Std(b []byte) string {
	const digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		sb.WriteByte(digits[v>>18&0x3f])
		sb.WriteByte(digits[v>>12&0x3f])
		if n > 1 {
			sb.WriteByte(digits[v>>6&0x3f])
		} else {
			sb.WriteByte('=')
		}
		if n > 2 {
			sb.WriteByte(digits[v&0x3f])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}

// randomHex n 字节随机 hex（Chat-Id / Session-Id 形态）。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func mustJSON(m map[string]json.RawMessage) []byte {
	raw, _ := json.Marshal(m)
	return raw
}
