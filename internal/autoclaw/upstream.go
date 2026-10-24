// upstream.go AutoClaw 上游客户端：userapi 链（签名头）+ 对话链（品牌头）。
// 覆盖刷新（一次性轮换 + 400002 降级 + 单飞）、签到、积分钱包、订阅、模型目录、
// 对话转发。
package autoclaw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/stagger"
)

// Upstream 上游客户端（对话不设总超时：只由调用方 request context 控制——
// 长流式响应可能超过常规 timeout）。
type Upstream struct {
	HTTP *http.Client
}

// NewUpstream 生产默认。
func NewUpstream() *Upstream {
	return &Upstream{HTTP: &http.Client{Timeout: 0}} // 总超时交给 context
}

// doJSON 发 JSON 请求（带签名头或品牌头由调用方给全）。
func (u *Upstream) doJSON(method, url string, headers [][2]string, payload any) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, 0, err
	}
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return raw, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// 业务信封（HTTP 200 + body {code, msg, data}）
// ---------------------------------------------------------------------------

// bizEnvelope {code,msg|message,data} 信封。code==0 成功。
type bizEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// errText msg/message 取先非空。
func (e *bizEnvelope) errText() string {
	if strings.TrimSpace(e.Msg) != "" {
		return strings.TrimSpace(e.Msg)
	}
	return strings.TrimSpace(e.Message)
}

// 业务码（agent2api 实测表）：
//
//	410000 / 400000 登录态失效；400002 签名校验失败（时钟漂移）；
//	400001 参数错误；630202 验证码错误；630101 发码过频；
//	631002 缺风控验证（误导性文案「当前版本已停止服务」）；630014 风控未通过；
//	631001 授权码无效；810001 pay-view 会员墙。
var (
	// ErrAuthInvalid 登录态失效（410000/400000）。
	ErrAuthInvalid = errors.New("登录态失效")
	// ErrSignRejected 签名校验失败（400002）。
	ErrSignRejected = errors.New("签名校验失败")
)

// authBizCode 登录态失效业务码判定。
func authBizCode(code int) bool { return code == 410000 || code == 400000 }

// ---------------------------------------------------------------------------
// 刷新（一次性轮换；refresh.rs:286-385）
// ---------------------------------------------------------------------------

// RefreshSingleFlight 无条件刷新（单飞；key=uid+refresh token 指纹防同 token 并发
// 双飞——双飞第二轮必 401）。
func (g *Gateway) RefreshSingleFlight(cred *Credential, force bool) (*Credential, error) {
	if !force && !cred.NeedsRefresh(time.Now()) {
		return cred, nil
	}
	if !cred.CanRefresh() {
		return nil, fmt.Errorf("账号没有 refresh token，无法续期：请重新登录或导入")
	}
	key := cred.SafeUID() + ":" + tokenTail(cred.RefreshToken)
	g.rf.mu.Lock()
	if f, ok := g.rf.flights[key]; ok {
		g.rf.mu.Unlock()
		<-f.done
		return f.cred, f.err
	}
	f := &flight{done: make(chan struct{})}
	g.rf.flights[key] = f
	g.rf.mu.Unlock()
	f.cred, f.err = g.refreshOnce(cred)
	close(f.done)
	g.rf.mu.Lock()
	delete(g.rf.flights, key)
	g.rf.mu.Unlock()
	return f.cred, f.err
}

// refreshOnce 实刷一次：POST /userapi/v1/refresh；400002 降级 /userapi/v1/agent-refresh。
func (g *Gateway) refreshOnce(cred *Credential) (*Credential, error) {
	userAPI := Endpoints(g.region).UserAPI
	refreshBody := map[string]any{
		"refresh_token": cred.RefreshToken,
		"source_id":     "autoclaw",
	}
	if cred.DeviceID != "" {
		refreshBody["device_id"] = cred.DeviceID
	}
	doRefresh := func(path string) ([]byte, int, error) {
		return g.upstream.doJSON(http.MethodPost, userAPI+path,
			signedAuthHeaders(cred.AccessToken), refreshBody)
	}
	raw, code, err := doRefresh("/userapi/v1/refresh")
	if err != nil {
		return nil, fmt.Errorf("刷新请求失败: %w", err)
	}
	var env bizEnvelope
	_ = json.Unmarshal(raw, &env)
	// 400002 = 签名校验失败（时钟漂移或指纹变动）→ 降级 agent-refresh 再试一次。
	if env.Code == 400002 {
		raw, code, err = doRefresh("/userapi/v1/agent-refresh")
		if err != nil {
			return nil, fmt.Errorf("降级刷新请求失败: %w", err)
		}
		_ = json.Unmarshal(raw, &env)
	}
	if code == http.StatusUnauthorized {
		return nil, ErrAuthInvalid
	}
	if authBizCode(env.Code) {
		return nil, ErrAuthInvalid
	}
	if env.Code == 400002 {
		return nil, fmt.Errorf("%w（时钟漂移？校验本机时间）", ErrSignRejected)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("刷新被拒（HTTP %d code %d）: %s", code, env.Code, env.errText())
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || strings.TrimSpace(data.AccessToken) == "" {
		return nil, fmt.Errorf("刷新响应缺少 access_token")
	}
	userID, deviceID, exp, err := ParseToken(data.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("刷新返回的 token 无法解析: %w", err)
	}
	// 一次性轮换：响应没给新 refresh token 时沿用旧值（refresh.rs:380-385）。
	nextRefresh := firstNonEmpty(data.RefreshToken, cred.RefreshToken)
	_ = userID
	if deviceID != "" && cred.DeviceID == "" {
		cred.DeviceID = deviceID
	}
	cred.UpdateTokens(data.AccessToken, nextRefresh, exp)
	if g.authDir != "" {
		if err := cred.SaveAtomic(g.authDir); err != nil {
			// 刷了不存 = 旧 refresh token 作废后账号死刑——记日志但不阻断本次对话。
			g.logf("⚠️ 刷新后写回凭证失败（refresh token 已轮换）: %v", err)
		}
	}
	g.upsert(cred)
	return cred, nil
}

// EnsureFresh 临期（或 force）刷新；不可刷新账号直接返回（匿名兜底语义不存在于
// 落库账号——没有 refresh token 的账号到期后只能重新导入）。
func (g *Gateway) EnsureFresh(cred *Credential, force bool) (*Credential, error) {
	if !cred.CanRefresh() {
		if force {
			return nil, fmt.Errorf("账号没有 refresh token，无法续期：请重新导入")
		}
		return cred, nil
	}
	return g.RefreshSingleFlight(cred, force)
}

// StartHourlyRefresh 每小时无条件强制刷一次（温着 refresh token 防长期闲置被判
// 失效；agent2api credentials.rs:170-207 同策略）。
//
// 风控口径（2026-10-24 整改）：与 CheckinAll 共用 checkinMu 互斥，避免两批刷新
// 交叉打同一账号；账号间 stagger.Heavy() 随机错峰，绝不批量同刻；整点触发先睡
// FireJitter 抹掉「每小时准点 hh:00:00 开刷」指纹。忙时直接丢一轮。
func (g *Gateway) StartHourlyRefresh(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				g.refreshAllHourly()
			}
		}
	}()
}

// refreshAllHourly 每小时强制刷新一轮（闸门 + 触发抖动 + 账号间错峰）。
func (g *Gateway) refreshAllHourly() {
	if !g.checkinMu.TryLock() {
		g.logf("每小时强制刷新：批量签到/刷新执行中，本轮跳过（闸门互斥）")
		return
	}
	defer g.checkinMu.Unlock()
	time.Sleep(stagger.FireJitter())
	first := true
	for _, c := range g.List() {
		if !c.CanRefresh() || !c.Enabled {
			continue
		}
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		if _, err := g.RefreshSingleFlight(c, true); err != nil {
			g.logf("每小时强制刷新失败 uid=%s: %v", c.SafeUID(), err)
		}
	}
}

// ---------------------------------------------------------------------------
// 对话转发（品牌头 + 三处改写已在 gateway 侧完成）
// ---------------------------------------------------------------------------

// ChatForward 发一次对话（流式），返回原始响应流。
func (u *Upstream) ChatForward(cred *Credential, routeModel string, payload []byte) (io.ReadCloser, error) {
	url := Endpoints(cred.Provider2Region()).Upstream + "/chat/completions"
	headers := chatHeaders(cred.AccessToken, routeModel, newRequestID(), [][2]string{})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	// 对话不设总超时（connect 30s / read 600s 语义由 transport 层承担）。
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("对话传输失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	return resp.Body, nil
}

// UpstreamError 非 2xx（带状态码与体，供分类）。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("上游返回 %d: %s", e.Status, truncateForLog(e.Body, 240))
}

// Provider2Region 凭据 provider 字段 → Region。
func (c *Credential) Provider2Region() Region {
	if c.Provider == string(RegionIntl) {
		return RegionIntl
	}
	return RegionCN
}

// ---------------------------------------------------------------------------
// 签到 + 活动福利触发 + 积分钱包 + 订阅
// ---------------------------------------------------------------------------

// CheckinResult 每日签到结果（checkin.rs:90）。
type CheckinResult struct {
	OK               bool    `json:"ok"`   // 本次真领到
	Already          bool    `json:"already"`
	Reward           float64 `json:"reward,omitempty"`
	Msg              string  `json:"msg,omitempty"`
}

// AccountCheckinResult 带账号标识的签到结果（CheckinAll 用；单账号结果内嵌）。
type AccountCheckinResult struct {
	UID              string         `json:"uid"`
	OK               bool           `json:"ok"`
	Already          bool           `json:"already,omitempty"`
	Reward           float64        `json:"reward,omitempty"`
	Msg              string         `json:"msg,omitempty"`
}

// Checkin 每日签到：POST /autoclaw-proxy/proxy/autoclaw-task-complete（body
// {"task_id":"daily_signin"}）。判「真领到」= reward_points>0 || (success &&
// !already_completed)——当天已领过返回 success:false + already:true（HTTP 200）。
func (g *Gateway) Checkin(cred *Credential) (*CheckinResult, error) {
	url := Endpoints(g.region).UserAPI + "/autoclaw-proxy/proxy/autoclaw-task-complete"
	raw, code, err := g.upstream.doJSON(http.MethodPost, url,
		signedAuthHeaders(cred.AccessToken), map[string]any{"task_id": "daily_signin"})
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("签到返回 HTTP %d", code)
	}
	var env bizEnvelope
	_ = json.Unmarshal(raw, &env)
	if authBizCode(env.Code) {
		return nil, ErrAuthInvalid
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("签到被拒（code %d）: %s", env.Code, env.errText())
	}
	var data struct {
		AlreadyCompleted bool    `json:"already_completed"`
		RewardPoints     float64 `json:"reward_points"`
		Success          bool    `json:"success"`
	}
	_ = json.Unmarshal(env.Data, &data)
	out := &CheckinResult{Already: data.AlreadyCompleted, Reward: data.RewardPoints}
	out.OK = data.RewardPoints > 0 || (data.Success && !data.AlreadyCompleted)
	switch {
	case out.OK:
		out.Msg = fmt.Sprintf("获得 %g 积分", data.RewardPoints)
	case data.AlreadyCompleted:
		out.Msg = "今日已签到"
	default:
		out.Msg = "暂无可领签到奖励"
	}
	return out, nil
}

// Wallet 钱包行（balance.rs:202-345）。
type Wallet struct {
	Type        string  `json:"type"`
	DisplayName string  `json:"display_name"`
	Balance     float64 `json:"balance"`
	BalanceView string  `json:"balance_view,omitempty"`
}

// Subscription 订阅快照（深搜候选键——上游结构不稳定）。
type Subscription struct {
	PlanName    string  `json:"planName,omitempty"`
	Status      string  `json:"status,omitempty"`
	ExpireAt    int64   `json:"expireAt,omitempty"`
	RemainQuota float64 `json:"remainQuota,omitempty"`
	TotalQuota  float64 `json:"totalQuota,omitempty"`
}

// BalanceResponse 余额查询输出。
type BalanceResponse struct {
	Total        float64       `json:"total"`
	Unit         string        `json:"unit"`
	Wallets      []Wallet      `json:"wallets"`
	Subscription *Subscription `json:"subscription,omitempty"`
}

// Balance 积分钱包 + 订阅。v2 → v1 兜底；登录态失效业务码归 ErrAuthInvalid。
func (g *Gateway) Balance(cred *Credential) (*BalanceResponse, error) {
	userAPI := Endpoints(g.region).UserAPI
	out := &BalanceResponse{Unit: "积分"}

	// v2 钱包。
	raw, code, err := g.upstream.doJSON(http.MethodGet,
		userAPI+"/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw",
		signedAuthHeaders(cred.AccessToken), nil)
	if err == nil && code == http.StatusOK {
		var env bizEnvelope
		_ = json.Unmarshal(raw, &env)
		if authBizCode(env.Code) {
			return nil, ErrAuthInvalid
		}
		if env.Code == 0 {
			var data struct {
				TotalBalance float64 `json:"total_balance"`
				Wallets      []struct {
					PublicWalletType string  `json:"public_wallet_type"`
					DisplayName      string  `json:"display_name"`
					Display          bool    `json:"display"`
					Balance          float64 `json:"balance"`
					BalanceView      string  `json:"balance_view"`
					Priority         int     `json:"priority"`
				} `json:"wallets"`
			}
			_ = json.Unmarshal(env.Data, &data)
			out.Total = data.TotalBalance
			for _, w := range data.Wallets {
				if !w.Display {
					continue
				}
				out.Wallets = append(out.Wallets, Wallet{
					Type: w.PublicWalletType, DisplayName: w.DisplayName,
					Balance: w.Balance, BalanceView: w.BalanceView,
				})
			}
		}
	}
	// v2 失败/空 → v1 兜底（wallet-instances 按 scope 归类）。
	if len(out.Wallets) == 0 {
		raw, code, err := g.upstream.doJSON(http.MethodGet,
			userAPI+"/agent-assetmgr/api/v1/wallet-instances?wallet_type=all&wallet_scope=all",
			signedAuthHeaders(cred.AccessToken), nil)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("钱包查询返回 HTTP %d", code)
		}
		var env bizEnvelope
		_ = json.Unmarshal(raw, &env)
		if authBizCode(env.Code) {
			return nil, ErrAuthInvalid
		}
		if env.Code != 0 {
			return nil, fmt.Errorf("钱包查询被拒（code %d）: %s", env.Code, env.errText())
		}
		var data struct {
			Wallets []struct {
				Scope       string  `json:"wallet_scope"`
				DisplayName string  `json:"display_name"`
				Status      string  `json:"status"`
				Display     bool    `json:"display"`
				Balance     float64 `json:"balance"`
			} `json:"wallets"`
		}
		_ = json.Unmarshal(env.Data, &data)
		scopeNames := map[string]string{
			"daily": "赠送积分", "monthly": "订阅积分",
			"long_term": "通用积分", "annual": "通用积分", "campaign": "活动积分",
		}
		for _, w := range data.Wallets {
			if w.Status != "active" || !w.Display {
				continue
			}
			name := w.DisplayName
			if name == "" {
				name = scopeNames[w.Scope]
			}
			if name == "" {
				name = w.Scope
			}
			out.Wallets = append(out.Wallets, Wallet{Type: w.Scope, DisplayName: name, Balance: w.Balance})
			out.Total += w.Balance
		}
	}
	// 订阅（best-effort：失败不阻塞钱包结果）。
	if sub, err := g.fetchSubscription(cred); err == nil {
		out.Subscription = sub
	}
	return out, nil
}

// fetchSubscription POST subscribe-info（深搜候选键）。
func (g *Gateway) fetchSubscription(cred *Credential) (*Subscription, error) {
	userAPI := Endpoints(g.region).UserAPI
	path := "/agentpay/v1/assistant/subscribe-info"
	body := map[string]any{"source_id": "autoclaw"}
	if cred.DeviceID != "" {
		body["device_id"] = cred.DeviceID
	}
	raw, code, err := g.upstream.doJSON(http.MethodPost, userAPI+path,
		signedAuthHeaders(cred.AccessToken), body)
	if err != nil || code != http.StatusOK {
		return nil, fmt.Errorf("subscribe HTTP %d", code)
	}
	var env bizEnvelope
	_ = json.Unmarshal(raw, &env)
	if authBizCode(env.Code) {
		return nil, ErrAuthInvalid
	}
	if env.Code != 0 || len(env.Data) == 0 {
		return nil, fmt.Errorf("subscribe code=%d", env.Code)
	}
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	// 容器深搜：subscribeInfo/subscribe/data/memberInfo/vipInfo。
	container := data
	for _, key := range []string{"subscribeInfo", "subscribe", "data", "memberInfo", "vipInfo"} {
		if m, ok := container[key].(map[string]any); ok {
			container = m
			break
		}
	}
	sub := &Subscription{
		PlanName:    strOfAny(firstKey(container, "productName", "planName", "memberName", "subscribeName", "vipName")),
		Status:      strOfAny(firstKey(container, "status", "subscribeStatus", "memberStatus")),
		RemainQuota: numOfAny(firstKey(container, "remainQuota", "remainingQuota", "remainCredits", "availableCredits", "leftQuota")),
		TotalQuota:  numOfAny(firstKey(container, "totalQuota", "credits", "totalCredits")),
	}
	for _, key := range []string{"expireAt", "expireTime", "expiredAt", "endTime", "validEndTime"} {
		if v, ok := container[key].(float64); ok && v > 0 {
			sub.ExpireAt = int64(v)
			break
		}
	}
	return sub, nil
}

func firstKey(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func strOfAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func numOfAny(v any) float64 {
	f, _ := v.(float64)
	return f
}

// TouchUserInfo 登录活跃触发（活动福利唯一入账动作；checkin.rs:45-55）。
func (g *Gateway) TouchUserInfo(cred *Credential) error {
	userAPI := Endpoints(g.region).UserAPI
	body := map[string]any{"source_id": "autoclaw"}
	if cred.DeviceID != "" {
		body["device_id"] = cred.DeviceID
	}
	raw, code, err := g.upstream.doJSON(http.MethodPost, userAPI+"/userapi/v1/user-info",
		signedAuthHeaders(cred.AccessToken), body)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("user-info HTTP %d", code)
	}
	var env bizEnvelope
	_ = json.Unmarshal(raw, &env)
	if authBizCode(env.Code) {
		return ErrAuthInvalid
	}
	return nil
}

// ---------------------------------------------------------------------------
// 手机验证码登录（国内版；login.rs）
// ---------------------------------------------------------------------------

// SendCode 发验证码。phone 为 11 位数字串。
func (g *Gateway) SendCode(phone string) (deviceID string, err error) {
	if !phoneRe.MatchString(strings.TrimSpace(phone)) {
		return "", fmt.Errorf("手机号格式不对（要 11 位数字）")
	}
	deviceID = NewDeviceID()
	userAPI := Endpoints(g.region).UserAPI
	raw, code, err := g.upstream.doJSON(http.MethodPost, userAPI+"/userapi/v1/agent-send-code",
		signedAuthHeaders(""), map[string]any{
			"phone": strings.TrimSpace(phone), "source_id": "autoclaw", "device_id": deviceID,
		})
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("发码返回 HTTP %d", code)
	}
	var env bizEnvelope
	_ = json.Unmarshal(raw, &env)
	if env.Code != 0 {
		return "", fmt.Errorf("发码被拒（code %d）: %s", env.Code, env.errText())
	}
	var data struct {
		Result bool `json:"result"`
	}
	_ = json.Unmarshal(env.Data, &data)
	if !data.Result {
		return "", fmt.Errorf("发码未确认（上游 result=false）")
	}
	return deviceID, nil
}

// phoneRe 11 位手机号。
var phoneRe = regexp.MustCompile(`^1\d{10}$`)

// VerifyCode 验证码登录（POST /userapi/v1/agent-login/——尾斜杠是上游要求）。
// code 必须 JSON 数字（传字符串稳定 400001）；两跳 device_id 必须一致。
func (g *Gateway) VerifyCode(deviceID, phone, code string) (*Credential, error) {
	codeNum := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(code), "%d", &codeNum); err != nil {
		return nil, fmt.Errorf("验证码要纯数字（%w）", err)
	}
	userAPI := Endpoints(g.region).UserAPI
	raw, httpCode, err := g.upstream.doJSON(http.MethodPost, userAPI+"/userapi/v1/agent-login/",
		signedAuthHeaders(""), map[string]any{
			"phone": strings.TrimSpace(phone), "code": codeNum, "platform": "web",
			"source_id": "autoclaw", "device_id": deviceID,
		})
	if err != nil {
		return nil, err
	}
	if httpCode != http.StatusOK {
		return nil, fmt.Errorf("登录返回 HTTP %d", httpCode)
	}
	var env bizEnvelope
	_ = json.Unmarshal(raw, &env)
	if env.Code != 0 {
		return nil, fmt.Errorf("登录被拒（code %d）: %s", env.Code, env.errText())
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.AccessToken == "" {
		return nil, fmt.Errorf("登录响应缺少 access_token")
	}
	cred, err := NewFromToken(data.AccessToken, data.RefreshToken, deviceID, "", "", "manual")
	if err != nil {
		return nil, err
	}
	return cred, nil
}

// ---------------------------------------------------------------------------
// 池操作（credential 列表 + 冷却）
// ---------------------------------------------------------------------------

type flight struct {
	done chan struct{}
	cred *Credential
	err  error
}

// cooldown 状态。
type cooldownState struct {
	mu     sync.Mutex
	untils map[string]time.Time
}

// newCooldownState 构造（untils 必须初始化——nil map 写入会 panic）。
func newCooldownState() cooldownState {
	return cooldownState{untils: map[string]time.Time{}}
}

func truncateForLog(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
