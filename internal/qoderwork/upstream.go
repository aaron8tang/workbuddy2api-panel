// upstream.go QoderWork 上游客户端：COSY 签名对话/模型列表 + Bearer(dt) 控制面 API。
// 移植自 qoderwork2api internal/upstream/{client,chat,models,cosy}.go，
// 签名/头铺设/编码收敛到 internal/cosy 共用底座（ProfileQoderWork 变体）。
package qoderwork

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// Client 上游客户端（HTTP/1.1 强制：gateway 对 HTTP/2 不友好，stream INTERNAL_ERROR）。
type Client struct {
	HTTP    *http.Client
	Base    string // https://openapi.qoder.com.cn
	Gateway string // https://gateway.qoder.com.cn
}

// NewClient 生产默认（180s 超时；连接池 MaxIdleConnsPerHost=20）。
func NewClient() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	p := cosy.ProfileQoderWork()
	return &Client{
		HTTP:    &http.Client{Timeout: 180 * time.Second, Transport: tr},
		Base:    p.OpenAPIBase,
		Gateway: p.GatewayBase,
	}
}

// identityOf 由账号 + 当前凭证组装 COSY identity（dt/drt 内嵌其中）。
func identityOf(acct *cosy.Account, cr *Cred) cosy.Identity {
	p := cosy.ProfileQoderWork()
	return cosy.Identity{
		Name:               cr.Nickname,
		Aid:                acct.UID,
		UID:                acct.UID,
		UserType:           p.AliyunUserType, // personal_professional_trial
		SecurityOAuthToken: cr.DT,
		RefreshToken:       cr.DRT,
	}
}

// ChatForward 构造 + 编码 + 签名后发到上游网关。
// 成功返回原始嵌套 SSE 流（调用方 Close）；失败返回 *cosy.Error（分类已定）。
func (c *Client) ChatForward(acct *cosy.Account, cr *Cred, openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) (io.ReadCloser, error) {
	if cr.DT == "" {
		return nil, &cosy.Error{Kind: cosy.ErrTokenExpired, Msg: "no dt- available"}
	}
	rawBody, err := BuildAgentBody(openaiMessages, modelKey, clientTools, reasoningEffort)
	if err != nil {
		return nil, fmt.Errorf("build body: %w", err)
	}
	encoded := cosy.EncodeBody(rawBody)

	rawURL := c.Gateway + cosy.ChatPath
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	sess, err := cosy.NewSession(acct.Fingerprint, identityOf(acct, cr))
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, cosy.ProfileQoderWork(), cosy.HeaderParams{
		Body:        encoded,
		RawURL:      rawURL,
		UID:         acct.UID,
		ModelKey:    modelKey,
		ModelSource: "system",
		SSE:         true,
	}); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, cosy.ClassifyError(resp.StatusCode, truncate(string(raw), 300))
	}
	return resp.Body, nil
}

// DynamicModel 上游 chat scene 单个模型。
type DynamicModel struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsVL           bool    `json:"is_vl"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	PriceFactor    float64 `json:"price_factor"`
}

// FetchModels 拉取上游动态模型列表（COSY 签名 GET，签名 body 恒为空串——
// 传 "{}" 会 403 Signature invalid）。
func (c *Client) FetchModels(acct *cosy.Account, cr *Cred) ([]DynamicModel, error) {
	if cr.DT == "" {
		return nil, fmt.Errorf("no dt- available")
	}
	rawURL := c.Gateway + cosy.ModelsPath
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	sess, err := cosy.NewSession(acct.Fingerprint, identityOf(acct, cr))
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, cosy.ProfileQoderWork(), cosy.HeaderParams{
		RawURL: rawURL,
		UID:    acct.UID,
		Accept: "application/json",
	}); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var apiResp map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	chatRaw, ok := apiResp["chat"]
	if !ok {
		return nil, fmt.Errorf("no chat scene in models response")
	}
	var models []DynamicModel
	if err := json.Unmarshal(chatRaw, &models); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	enabled := make([]DynamicModel, 0, len(models))
	for _, m := range models {
		if m.Enable && m.Key != "" {
			enabled = append(enabled, m)
		}
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	return enabled, nil
}

// ---------------------------------------------------------------------------
// 控制面 API（Bearer dt，无 COSY 签名）
// ---------------------------------------------------------------------------

func (c *Client) get(path, dt string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	cosy.ApplyControlHeaders(req, cosy.ProfileQoderWork(), dt, false)
	return c.HTTP.Do(req)
}

func (c *Client) post(path, dt string, body []byte) (*http.Response, error) {
	if body == nil {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	cosy.ApplyControlHeaders(req, cosy.ProfileQoderWork(), dt, true)
	return c.HTTP.Do(req)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// QuotaUsage 查询积分余额（基础 + 赠送聚合）。
// 内部改调 QuotaDetail（三桶口径），remaining 相加保池的 SetCredits 语义不变。
func (c *Client) QuotaUsage(dt string) (remain int64, exceeded bool, err error) {
	detail, err := c.QuotaDetail(dt)
	if err != nil {
		return 0, false, err
	}
	return int64(detail.Available()), detail.Exceeded, nil
}

// QuotaBucket 单桶（userQuota / addOnQuota）。
type QuotaBucket struct {
	Total     *float64 `json:"total"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit,omitempty"`
}

// OrgBucket orgResourcePackage：总量叫 cap（旧形状 total 兜底）、另有 available 布尔
//（空桶也整个键返回，键在不在不能当判据）。
type OrgBucket struct {
	Cap       *float64 `json:"cap"`
	Total     *float64 `json:"total"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit,omitempty"`
	Available *bool    `json:"available"`
}

// QuotaDetail 三桶余额（agent2api qoder/balance.rs 口径）。
type QuotaDetail struct {
	User     QuotaBucket `json:"userQuota"`
	AddOn    *QuotaBucket `json:"addOnQuota"`
	Org      *OrgBucket  `json:"orgResourcePackage"`
	Exceeded bool        `json:"isQuotaExceeded"`
	// ExpiresAt 订阅到期毫秒；哨兵 253402214400000（9999-12-31）按无到期处理。
	ExpiresAt *float64 `json:"expiresAt"`
}

// Available 汇总：user.remaining + addOn.remaining + org.remaining(仅 available:true)。
// 三项任一缺失不伪造 0，全缺返回 0（调用方以明细为准）。
func (q *QuotaDetail) Available() float64 {
	var sum float64
	any := false
	if q.User.Remaining != nil {
		sum += *q.User.Remaining
		any = true
	}
	if q.AddOn != nil && q.AddOn.Remaining != nil {
		sum += *q.AddOn.Remaining
		any = true
	}
	if q.Org != nil && q.Org.Remaining != nil && q.OrgUsable() {
		sum += *q.Org.Remaining
		any = true
	}
	if !any {
		return 0
	}
	return sum
}

// OrgUsable 组织桶可用判定：available 缺失（旧形状）退「cap/used/remaining 任一 >0」。
func (q *QuotaDetail) OrgUsable() bool {
	if q.Org == nil {
		return false
	}
	if q.Org.Available != nil {
		return *q.Org.Available
	}
	for _, v := range []*float64{q.Org.Cap, q.Org.Used, q.Org.Remaining} {
		if v != nil && *v > 0 {
			return true
		}
	}
	return false
}

// AddOnUsable 个人资源包判据：键在 + 非全零（上游没包时整个键缺失或全零）。
func (q *QuotaDetail) AddOnUsable() bool {
	if q.AddOn == nil {
		return false
	}
	for _, v := range []*float64{q.AddOn.Total, q.AddOn.Used, q.AddOn.Remaining} {
		if v != nil && *v > 0 {
			return true
		}
	}
	return false
}

// QuotaDetail 查询三桶余额（数字容忍字符串形态）。
func (c *Client) QuotaDetail(dt string) (*QuotaDetail, error) {
	resp, err := c.get("/api/v2/quota/usage", dt)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("quota http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic map[string]any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("quota parse: %w", err)
	}
	numPtr := func(v any) *float64 {
		switch t := v.(type) {
		case json.Number:
			f, err := t.Float64()
			if err != nil {
				return nil
			}
			return &f
		case string:
			var f float64
			if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err == nil {
				return &f
			}
		}
		return nil
	}
	detail := &QuotaDetail{}
	if b, ok := generic["userQuota"].(map[string]any); ok {
		detail.User = QuotaBucket{Total: numPtr(b["total"]), Used: numPtr(b["used"]),
			Remaining: numPtr(b["remaining"])}
		if s, _ := b["unit"].(string); s != "" {
			detail.User.Unit = s
		}
	}
	if b, ok := generic["addOnQuota"].(map[string]any); ok {
		detail.AddOn = &QuotaBucket{Total: numPtr(b["total"]), Used: numPtr(b["used"]),
			Remaining: numPtr(b["remaining"])}
		if s, _ := b["unit"].(string); s != "" {
			detail.AddOn.Unit = s
		}
	}
	if b, ok := generic["orgResourcePackage"].(map[string]any); ok {
		org := &OrgBucket{Cap: numPtr(b["cap"]), Total: numPtr(b["total"]),
			Used: numPtr(b["used"]), Remaining: numPtr(b["remaining"])}
		if av, ok := b["available"].(bool); ok {
			org.Available = &av
		}
		if s, _ := b["unit"].(string); s != "" {
			org.Unit = s
		}
		detail.Org = org
	}
	detail.Exceeded, _ = generic["isQuotaExceeded"].(bool)
	if v, ok := generic["expiresAt"]; ok && v != nil {
		detail.ExpiresAt = numPtr(v)
	}
	return detail, nil
}

// ---------------------------------------------------------------------------
// sash 签到（campaigns CLAIM_BENEFIT）：与 openapi 同主机但鉴权口径完全不同 ——
// 只要 Bearer + cosy-clienttype:10 + UA Qoder，不打 COSY 签名。
// legacy /daily-check-in/claim|status 已被上游全局停用（对未领日子也 409 不发
// 积分），一次都不该碰。
// ---------------------------------------------------------------------------

// sashHeaders 活动/签到接口请求头（照抄抓包：clienttype 门控返回内容，
// clienttype=5 打活动接口返回 showCampaign:false + 空列表，换 10 才给完整活动包）。
func (c *Client) sashHeaders(dt string, post bool) [][2]string {
	hs := [][2]string{
		{"authorization", "Bearer " + dt},
		{"cosy-clienttype", "10"},
		{"accept", "application/json"},
		{"accept-language", "zh-CN"},
		{"user-agent", "Qoder"},
	}
	if post {
		hs = append(hs, [][2]string{{"content-type", "application/json"}, {"origin", c.Base}}...)
	}
	return hs
}

// Campaign 一条签到活动。
type Campaign struct {
	CampaignID  string  `json:"campaignId"`
	ActionType  string  `json:"actionType"`
	ClaimStatus string  `json:"claimStatus"`
	Amount      float64 `json:"amount"`
	Days        float64 `json:"days"`
}

// campaignsResp 活动列表响应。
type campaignsResp struct {
	UID       string `json:"uid"`
	Campaigns []struct {
		CampaignID  string `json:"campaignId"`
		ActionType  string `json:"actionType"`
		ClaimStatus string `json:"claimStatus"`
		Benefit     struct {
			Kind     string `json:"kind"`
			Amount   float64 `json:"amount"`
			Validity struct {
				Mode string  `json:"mode"`
				Days float64 `json:"days"`
			} `json:"validity"`
		} `json:"benefit"`
	} `json:"campaigns"`
}

// CheckinOutcome 签到结果。
type CheckinOutcome struct {
	OK      bool    // 本次领取成功
	Already bool    // 今天已领（409 / ALREADY_CLAIMED / replayed）
	NoClaim bool    // 没有可领活动（区别于报错）
	Reward  float64 // 领到的 credits（缺省 100）
	Days    float64 // 有效期天数
	Msg     string
}

// sashDo 活动/签到请求。
func (c *Client) sashDo(method, path string, dt string, post bool) ([]byte, int, error) {
	req, err := http.NewRequest(method, c.Base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	for _, kv := range c.sashHeaders(dt, post) {
		req.Header.Set(kv[0], kv[1])
	}
	if post {
		req.Body = io.NopCloser(strings.NewReader("{}"))
		req.ContentLength = 2
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := readBody(resp)
	return raw, resp.StatusCode, err
}

// CheckinV2 campaigns 签到：列表 → 筛 CLAIM_BENEFIT → 领。中国版不需要 UMID
// 风控头/两段拉取（那是国际版专属）。
func (c *Client) CheckinV2(dt string) (*CheckinOutcome, error) {
	raw, code, err := c.sashDo(http.MethodGet, "/sash/api/v1/me/campaigns?forceRefresh=true", dt, false)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("campaigns http %d: %s", code, truncate(string(raw), 200))
	}
	var list campaignsResp
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("campaigns parse: %w", err)
	}
	// 顶层的 claimable 标志不能当判据（领完仍可能 true——同账号还有可领促销）。
	var target string
	claimed := false
	for _, row := range list.Campaigns {
		if row.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch row.ClaimStatus {
		case "CLAIMABLE":
			if target == "" {
				target = row.CampaignID
			}
		case "CLAIMED":
			claimed = true
		}
	}
	if target == "" {
		return &CheckinOutcome{Already: claimed, NoClaim: !claimed,
			Msg: ifStr2(map[bool]string{true: "今日已领取", false: "暂无可领取活动"}[claimed])}, nil
	}
	claimPath := "/sash/api/v1/me/campaigns/" + target + "/claim"
	craw, ccode, cerr := c.sashDo(http.MethodPost, claimPath, dt, true)
	if cerr != nil {
		return nil, cerr
	}
	var body map[string]any
	_ = json.Unmarshal(craw, &body)
	// 409 或 result=="ALREADY_CLAIMED"（上游并发/重放）= 今天已经领过，不算失败。
	result, _ := body["result"].(string)
	if ccode == http.StatusConflict || result == "ALREADY_CLAIMED" {
		return &CheckinOutcome{Already: true, Msg: "今日已领取"}, nil
	}
	if ccode >= 400 {
		return nil, fmt.Errorf("claim http %d: %s", ccode, truncate(string(craw), 200))
	}
	status, _ := body["status"].(string)
	if status != "CLAIMED" {
		return &CheckinOutcome{Msg: fmt.Sprintf("签到未完成（上游状态 %s）", status)}, nil
	}
	if replayed, _ := body["replayed"].(bool); replayed {
		return &CheckinOutcome{Already: true, Msg: "今日已领取"}, nil
	}
	out := &CheckinOutcome{OK: true, Reward: 100}
	if benefit, ok := body["benefit"].(map[string]any); ok {
		if amount, ok := benefit["amount"].(json.Number); ok {
			if f, e := amount.Float64(); e == nil && f > 0 {
				out.Reward = f
			}
		}
		if validity, ok := benefit["validity"].(map[string]any); ok {
			if days, ok := validity["days"].(json.Number); ok {
				if f, e := days.Float64(); e == nil {
					out.Days = f
				}
			}
		}
	}
	if out.Days > 0 {
		out.Msg = fmt.Sprintf("获得 %g credits，%g 天有效", out.Reward, out.Days)
	} else {
		out.Msg = fmt.Sprintf("获得 %g credits", out.Reward)
	}
	return out, nil
}

func ifStr2(s string) string {
	return s
}

// DailyCheckin 兼容旧签名：true=本次成功；false=已签/失败。
func (c *Client) DailyCheckin(dt string) (bool, error) {
	out, err := c.CheckinV2(dt)
	if err != nil {
		return false, err
	}
	return out.OK, nil
}

// CheckinStatus 签到状态。
type CheckinStatus struct {
	Status             string `json:"status"` // CLAIMABLE | CLAIMED
	RewardCredits      int64  `json:"rewardCredits"`
	NextClaimAt        int64  `json:"nextClaimAt"`
	CurrentStreakDays  int64  `json:"currentStreakDays"`
	TotalClaimDays     int64  `json:"totalClaimDays"`
	TotalRewardCredits int64  `json:"totalRewardCredits"`
	LastClaimedAt      int64  `json:"lastClaimedAt"`
	RewardExpiresAt    int64  `json:"rewardExpiresAt"`
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(dt string) (*CheckinStatus, error) {
	resp, err := c.get("/sash/api/v1/me/daily-check-in/status", dt)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("checkin status http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var st CheckinStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("checkin status parse: %w", err)
	}
	return &st, nil
}

// UserPlan 套餐名（plan_tier_name 优先，回落 user_type）。
func (c *Client) UserPlan(dt string) (string, error) {
	resp, err := c.get("/api/v2/user/plan", dt)
	if err != nil {
		return "", err
	}
	raw, err := readBody(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("plan http %d", resp.StatusCode)
	}
	var p struct {
		UserType     string `json:"user_type"`
		PlanTierName string `json:"plan_tier_name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if p.PlanTierName != "" {
		return p.PlanTierName, nil
	}
	return p.UserType, nil
}

// UserInfo 用户信息。
type UserInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// UserInfo 查询用户信息。
func (c *Client) UserInfo(dt string) (*UserInfo, error) {
	resp, err := c.get("/api/v1/userinfo", dt)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo http %d", resp.StatusCode)
	}
	var u UserInfo
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ResolveModelMap 动态模型 → 客户端模型映射（客户端名 = display_name 规范化，
// 无 display_name 用 key 兜底）。返回：客户端名 → 上游 key。
func ResolveModelMap(dynamic []DynamicModel) map[string]string {
	out := make(map[string]string, len(dynamic))
	for _, m := range dynamic {
		name := m.Key
		if m.DisplayName != "" {
			name = NormalizeModelName(m.DisplayName)
		}
		out[name] = m.Key
	}
	return out
}

// NormalizeModelName 把 display_name 转成 OpenAI 风格客户端名：
// 小写、空格/下划线转连字符、保留点号（版本号）、保留中文、去重连字符。
func NormalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// 其他字符（中文等）原样保留
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
