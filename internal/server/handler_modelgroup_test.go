package server

// 模型组路由测试：/{group}/v1/*（及 /v1/{group}/* 别名）的组解析、模型校验、
// 默认模型按序选取、组内账号按序选号与"钉死不回落"语义。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/modelgroup"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
	traeauth "github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	traepool "github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
	traeupstream "github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// recordUpstream 记录每次出站请求的 Authorization（账号识别）与出站 body 的
// model / reasoning_effort 字段（模型重写与思考强度注入断言）。聊天行为恒 200 SSE 成功。
type recordUpstream struct {
	up      *upstream.Client
	mu      sync.Mutex
	tokens  []string
	models  []string
	efforts []string
}

func newRecordUpstream(t *testing.T) *recordUpstream {
	t.Helper()
	r := &recordUpstream{}
	r.up = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			r.mu.Lock()
			r.tokens = append(r.tokens, req.Header.Get("Authorization"))
			var peek struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
			}
			if req.Body != nil {
				raw, _ := io.ReadAll(req.Body)
				_ = json.Unmarshal(raw, &peek)
			}
			r.models = append(r.models, peek.Model)
			r.efforts = append(r.efforts, peek.ReasoningEffort)
			r.mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return r
}

func (r *recordUpstream) snapshot() (tokens, models, efforts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.tokens...), append([]string{}, r.models...), append([]string{}, r.efforts...)
}

func newGroupHandler(p *pool.Pool, up *upstream.Client, groups []modelgroup.Group) *Handler {
	return NewHandler(Config{
		Pool:        p,
		Upstream:    up,
		ModelGroups: modelgroup.NewRegistry(groups),
	})
}

func postChat(h *Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, bytes.NewReader([]byte(body))))
	return rec
}

// TestChatUnknownModelGroup 未知组 → 404（OpenAI 错误信封），主路由不受影响。
func TestChatUnknownModelGroup(t *testing.T) {
	up := newRecordUpstream(t)
	h := newGroupHandler(testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		up.up, nil)

	rec := postChat(h, "/dev/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group: got %d, want 404", rec.Code)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error.Code != "unknown_model_group" {
		t.Fatalf("error code = %q, want unknown_model_group", out.Error.Code)
	}
	// /v1/{group} 别名形态同判。
	if rec := postChat(h, "/v1/dev/chat/completions", `{"model":"glm-5.2","messages":[]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("alias form unknown group: got %d, want 404", rec.Code)
	}
	// 主路由照常（组缺失零回归）。
	if rec := postChat(h, "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("main route broken: got %d, want 200", rec.Code)
	}
}

// TestChatGroupModelWhitelist 组钉了模型清单时，清单外模型 → 400 model_not_in_group。
func TestChatGroupModelWhitelist(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.2", "cn:deepseek-v4-flash"}},
	})

	rec := postChat(h, "/dev/v1/chat/completions", `{"model":"kimi-k3","messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-member model: got %d, want 400", rec.Code)
	}
	var out struct {
		Error struct {
			Code        string `json:"code"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error.Code != "model_not_in_group" {
		t.Fatalf("error code = %q, want model_not_in_group", out.Error.Code)
	}
	if !strings.Contains(out.Error.GatewayHint, "glm-5.2") {
		t.Fatalf("hint should list allowed models, got %q", out.Error.GatewayHint)
	}
	// 白名单内模型照常出站（出站 body model 为裸名）。
	rec = postChat(h, "/dev/v1/chat/completions", `{"model":"cn:glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("member model: got %d, want 200", rec.Code)
	}
	_, models, _ := up.snapshot()
	if len(models) != 1 || models[0] != "glm-5.2" {
		t.Fatalf("outbound model = %v, want [glm-5.2] (bare, prefix stripped)", models)
	}
}

// TestChatGroupDefaultModel 组内模型顺序 = 默认模型优先级：model 缺省、等于组名、
// 或字面量 "default" 时按序取第一个可用模型，并把请求体 model 改写为该值。
func TestChatGroupDefaultModel(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:deepseek-v4-flash", "cn:glm-5.2"}},
	})

	for _, body := range []string{
		`{"messages":[]}`,                   // model 缺省
		`{"model":"dev","messages":[]}`,     // model = 组名
		`{"model":"default","messages":[]}`, // model = "default"（等同缺省）
	} {
		rec := postChat(h, "/dev/v1/chat/completions", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %s: got %d, want 200", body, rec.Code)
		}
	}
	_, models, _ := up.snapshot()
	if len(models) != 3 {
		t.Fatalf("outbound calls = %v, want 3", models)
	}
	for i, m := range models {
		if m != "deepseek-v4-flash" {
			t.Fatalf("outbound models[%d] = %q, want deepseek-v4-flash (first in group order)", i, m)
		}
	}
}

// TestChatGroupAccountOrder 组钉了账号清单：按序选号（u2 优先于 u1）。
func TestChatGroupAccountOrder(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.2"}, Accounts: []string{"u2", "u1"}},
	})

	rec := postChat(h, "/dev/v1/chat/completions", `{"model":"cn:glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	tokens, _, _ := up.snapshot()
	if len(tokens) != 1 || !strings.HasSuffix(tokens[0], "at2") {
		t.Fatalf("first pick should be u2 (group order), got %v", tokens)
	}
}

// TestChatGroupAccountPinnedNoFallback 组内账号全部不可用（此处：禁用）→ 503，
// 不回落到清单外的健康账号（钉死语义）。
func TestChatGroupAccountPinnedNoFallback(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.2"}, Accounts: []string{"u2"}},
	})
	p.Disable("u2", "test disable")

	rec := postChat(h, "/dev/v1/chat/completions", `{"model":"cn:glm-5.2","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 (pinned account unavailable, no fallback)", rec.Code)
	}
	tokens, _, _ := up.snapshot()
	if len(tokens) != 0 {
		t.Fatalf("no upstream call expected, got %v", tokens)
	}
}

// TestChatGroupTraeEntries 组内 trae: 前缀条目（阶段 2b 合并）：
//   - 渠道关闭（Trae==nil）：model=default 跳过 trae 条目、回落组内 workbuddy 模型
//     （default 不落在不可服务渠道上）；点名 trae: 模型过组白名单（裸名判等）后
//     按渠道关闭 404。
//   - 渠道开启但无可服务账号：default 跳过 trae 条目回落 cn（到期序会让 trae 频繁
//     排前，Servable 门控保证 trae 全冷却时不卡死 default）；点名 trae: 模型仍整体
//     分发进 trae 网关 → 503 no_healthy_account（渠道状态如实透传）。
func TestChatGroupTraeEntries(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	groups := []modelgroup.Group{
		// trae 条目写在最前：两模型倍率均未知（假上游无目录）→ 倍率次序退化书写顺序，
		// 覆盖「default 命中 trae 条目」与「跳过 trae 回落 cn 条目」两条路径。
		{Name: "dev", Models: []string{"trae:glm-5.2", "cn:glm-5.2"}},
	}

	// 渠道关闭：default 回落 cn 条目（跳过 trae），出站 model 为裸名。
	h := newGroupHandler(p, up.up, groups)
	rec := postChat(h, "/dev/v1/chat/completions", `{"model":"default","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("default with trae disabled: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	_, models, _ := up.snapshot()
	if len(models) != 1 || models[0] != "glm-5.2" {
		t.Fatalf("default should fall back to cn entry (skip trae), outbound models = %v", models)
	}

	// 渠道关闭：点名 trae: 模型 → 404 unknown_model（主链路零回归语义）。
	rec = postChat(h, "/dev/v1/chat/completions", `{"model":"trae:glm-5.2","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("explicit trae model with channel disabled: got %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}

	// 渠道开启但无账号：default 跳过 trae 回落 cn 条目 → 200。
	tp := traepool.New("")
	h2 := NewHandler(Config{
		Pool:        p,
		Upstream:    up.up,
		ModelGroups: modelgroup.NewRegistry(groups),
		Trae:        trae.New(trae.Config{Pool: tp, Upstream: traeupstream.New(), WorkMode: "disabled"}),
	})
	rec = postChat(h2, "/dev/v1/chat/completions", `{"model":"default","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("default with trae unservable: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, models, _ = up.snapshot(); len(models) != 2 || models[1] != "glm-5.2" {
		t.Fatalf("default should skip unservable trae entry, outbound models = %v", models)
	}

	// 点名 trae: 模型仍整体分发进 trae 网关 → 503 no_healthy_account。
	rec = postChat(h2, "/dev/v1/chat/completions", `{"model":"trae:glm-5.2","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("explicit trae model: got %d, want 503 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Fatalf("expect trae gateway error no_healthy_account, got %s", rec.Body.String())
	}
}

// TestChatGroupDefaultFallsToTraeWhenPoolEmpty 主池 cn 域无健康账号（用户全停
// workbuddy 账号、只留 trae 渠道）时，model=default / 缺省应跳过全部 cn 条目、
// 落到组内 trae 条目并整体分发进 trae 网关（组内多渠道容灾）——而不是选中 cn
// 模型后在主池报 no_healthy_account。trae 网关持有健康账号（Servable=true），
// 上游指向本地假服务器（恒 500）→ 网关轮转耗尽后如实报错，零真实网络。
func TestChatGroupDefaultFallsToTraeWhenPoolEmpty(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Disable("u1", "stopped by user")
	groups := []modelgroup.Group{
		// cn 条目倍率已知（假上游目录）排前、trae 条目倍率未知排最后——覆盖
		//「优先级靠前的 cn 条目全被渠道可服务性跳过后 default 落到末位 trae 条目」。
		{Name: "dev", Models: []string{"cn:glm-5.2", "trae:glm-5.2"}},
	}
	// trae 假上游：RoundTripper 注入恒 500（不起真实 listener，本机沙箱绑端口
	// 会挂死），让网关自己走轮转/冷却状态机后报 no_healthy_account。
	tu := traeupstream.New()
	fakeTrae := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"internal error"}`)),
		}, nil
	})}
	tu.HTTP = fakeTrae
	tu.StreamHTTP = fakeTrae
	tp := traepool.New("")
	tp.Add(&traeauth.Auth{UID: "t1", AccessToken: "tat", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:        p,
		Upstream:    up.up,
		ModelGroups: modelgroup.NewRegistry(groups),
		Trae:        trae.New(trae.Config{Pool: tp, Upstream: tu, WorkMode: "disabled"}),
	})

	for _, body := range []string{
		`{"messages":[]}`,                   // model 缺省
		`{"model":"default","messages":[]}`, // model = "default"
	} {
		rec := postChat(h, "/dev/v1/chat/completions", body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("body %s: got %d, want 503 (body: %s)", body, rec.Code, rec.Body.String())
		}
		// 错误必须来自 trae 网关（无 gateway_hint），而非主池（主池 503 带
		// gateway_hint "no healthy account available in pool"）——证明 default
		// 确实分发进了 trae 渠道而不是留在主链路。
		if strings.Contains(rec.Body.String(), "gateway_hint") {
			t.Fatalf("body %s: default should dispatch to trae gateway, got main-pool error: %s", body, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "no_healthy_account") {
			t.Fatalf("body %s: expect trae gateway error no_healthy_account, got %s", body, rec.Body.String())
		}
	}
	// 主链路不应有任何出站调用（cn 账号已全停，default 也不该选中 cn 模型）。
	if tokens, _, _ := up.snapshot(); len(tokens) != 0 {
		t.Fatalf("no main-pool outbound call expected, got %v", tokens)
	}
}

// TestChatGroupDefaultByExpiry 跨渠道「谁先到期用谁」default 排序（单元级，只断言
// groupDefaultModel 的选中结果，分发路径由 TestChatGroupTraeEntries /
// TestChatGroupDefaultFallsToTraeWhenPoolEmpty 覆盖）：
//   - trae 重置时刻早于 workbuddy 最早到期批次 → default 落 trae 条目；
//   - workbuddy 批次先到期 → default 回 cn 条目（倍率未知不再天然压过 trae）；
//   - trae 渠道到期信号未知（重置日关闭）→ 退化为原倍率/书写序（cn 条目优先）。
func TestChatGroupDefaultByExpiry(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	groups := []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.2", "trae:glm-5.2"}},
	}
	tp := traepool.New("")
	tp.Add(&traeauth.Auth{UID: "t1", AccessToken: "tat", ExpiresAt: 9999999999})
	// 重置日取「今天+10 天」的日期号：nextMonthlyReset 恒在 ~9-11 天后，消除
	// 月末边界（若固定 1 号且测试在月末深夜跑，重置时刻会比 +24h 的包还早，
	// 「谁先到期」断言就不成立了）。
	gw := trae.New(trae.Config{
		Pool: tp, Upstream: traeupstream.New(), WorkMode: "disabled",
		CreditResetDay: time.Now().AddDate(0, 0, 10).Day(),
	})
	reg := modelgroup.NewRegistry(groups)
	h := NewHandler(Config{Pool: p, Upstream: up.up, ModelGroups: reg, Trae: gw})
	g, ok := reg.Lookup("dev")
	if !ok {
		t.Fatal("group dev missing")
	}

	traeExp, ok := gw.EarliestCreditExpiry()
	if !ok {
		t.Fatal("trae expiry should be known (servable + reset day set)")
	}

	// trae 先到期：workbuddy 批次晚于 trae 重置 48h → default 落 trae 条目。
	p.SetCreditsDetailed("u1", 1000, 0, 0, traeExp.Add(48*time.Hour), 1000)
	if m, _ := h.groupDefaultModel(&g); m != "trae:glm-5.2" {
		t.Fatalf("trae expiring sooner: default = %q, want trae:glm-5.2", m)
	}

	// workbuddy 先到期：批次取 now 与 trae 重置的中点（恒早于 trae、恒在未来）。
	cnExp := time.Now().Add(time.Until(traeExp) / 2)
	p.SetCreditsDetailed("u1", 1000, 0, 0, cnExp, 1000)
	if m, _ := h.groupDefaultModel(&g); m != "cn:glm-5.2" {
		t.Fatalf("workbuddy expiring sooner: default = %q, want cn:glm-5.2", m)
	}

	// trae 到期信号未知（重置日关闭）：回到倍率序——两模型倍率均未知时保持书写
	// 顺序，cn 条目在前。
	h2 := NewHandler(Config{
		Pool:        p,
		Upstream:    up.up,
		ModelGroups: reg,
		Trae:        trae.New(trae.Config{Pool: tp, Upstream: traeupstream.New(), WorkMode: "disabled", CreditResetDay: 0}),
	})
	if m, _ := h2.groupDefaultModel(&g); m != "cn:glm-5.2" {
		t.Fatalf("trae expiry unknown: default = %q, want cn:glm-5.2 (rate/config order)", m)
	}
}

// TestChatGroupDefaultTraeBoostOnExpiring24h 主池多个健康账号都有 24h 内到期
// 积分（pkgs 快照口径，含消耗实时扣减）时，跨渠道排序 boost：trae 条目整体排
// 到 cn 条目之前（CountExpiring24h >= 2），default 也应落在 trae 条目上。
// 单账号有 24h 内到期（< 2）不 boost——「几个」取最小合理解释为 2。
func TestChatGroupDefaultTraeBoostOnExpiring24h(t *testing.T) {
	up := newRecordUpstream(t)
	// 两个主池账号：u1/u2 都有 24h 内到期包——满足 CountExpiring24h >= 2；u3
	// 无包（对照组）。包用 now 相对偏移构造（+8h / +30h），窗口归零仍恒在未来。
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	now := time.Now()
	pkgAt := func(remind time.Duration) []pool.PkgSnapshot {
		return []pool.PkgSnapshot{{Remain: 500, ExpireAt: now.Add(remind)}}
	}
	// 明天/后天的固定钟点（时刻无关构造）：now+30h 在 now>18:00 时落到后天
	// 凌晨（dDays=2 被天粒度窗口排除）导致傍晚后跑测试翻车；改用「明天 12:00」
	// 与「后天 12:00」的绝对时刻，任何时刻运行都恒为 dDays=1 / dDays=2。
	tomorrowNoon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	dayAfterNoon := tomorrowNoon.AddDate(0, 0, 1)
	p.SetCreditPackages("u1", pkgAt(8*time.Hour)) // 24h 窗口内
	p.SetCreditPackages("u2", []pool.PkgSnapshot{{Remain: 500, ExpireAt: tomorrowNoon}})
	p.SetCreditPackages("u3", []pool.PkgSnapshot{{Remain: 500, ExpireAt: dayAfterNoon}}) // d=2：不在 1 天窗口
	// 聚合到期信号（EarliestCreditExpiry 的数据源）与包时刻对齐：cn 渠道恒有
	// 已知到期且早于 trae 重置估算（~9-11 天后），boost 关闭后「谁先到期用谁」
	// 恒选 cn——测试只让 pkgs 计数驱动 boost 开关，其余键保持受控。
	p.SetCreditsDetailed("u1", 1000, 0, 0, now.Add(8*time.Hour), 1000)
	p.SetCreditsDetailed("u2", 1000, 0, 0, tomorrowNoon, 1000)
	p.SetCreditsDetailed("u3", 1000, 0, 0, dayAfterNoon, 1000)

	groups := []modelgroup.Group{
		// cn 条目写在前：boost 前它靠书写序/到期序可能排前；boost 后 trae 必须压过它。
		{Name: "dev", Models: []string{"cn:glm-5.2", "trae:glm-5.2"}},
	}
	tp := traepool.New("")
	tp.Add(&traeauth.Auth{UID: "t1", AccessToken: "tat", ExpiresAt: 9999999999})
	// 重置日取「今天+10 天」的日期号（理由同 TestChatGroupDefaultByExpiry）：
	// trae 重置估算恒在 ~9-11 天后，远晚于 u1/u2 的窗口内包，月末跑也不翻车。
	gw := trae.New(trae.Config{
		Pool: tp, Upstream: traeupstream.New(), WorkMode: "disabled",
		CreditResetDay: time.Now().AddDate(0, 0, 10).Day(),
	})
	reg := modelgroup.NewRegistry(groups)
	h := NewHandler(Config{Pool: p, Upstream: up.up, ModelGroups: reg, Trae: gw})
	g, ok := reg.Lookup("dev")
	if !ok {
		t.Fatal("group dev missing")
	}

	// boost 生效：主池两个账号都有 24h 内到期 → default 落 trae 条目。
	if n := p.CountExpiring24h(""); n != 2 {
		t.Fatalf("CountExpiring24h = %d, want 2", n)
	}
	if m, _ := h.groupDefaultModel(&g); m != "trae:glm-5.2" {
		t.Fatalf("boost active: default = %q, want trae:glm-5.2", m)
	}

	// 扣掉 u1 的包（模拟消耗扣空）：只剩 1 个账号有 24h 内到期 → 不再 boost，
	// 退回「谁先到期用谁」。u1 批次（今晚 23:00）恒早于 trae 重置估算（次月
	// 1 日 0 点）→ cn 先到期，default 回 cn 条目。
	p.SetCreditPackages("u1", nil)
	if n := p.CountExpiring24h(""); n != 1 {
		t.Fatalf("CountExpiring24h after drain u1 = %d, want 1", n)
	}
	if m, _ := h.groupDefaultModel(&g); m != "cn:glm-5.2" {
		t.Fatalf("boost off (<2 accounts): default = %q, want cn:glm-5.2 (expiry order)", m)
	}

	// u2 的包也扣掉：无窗口内到期账号 → 依旧 cn 条目（u2 明天 12:00 仍早于
	// trae 重置，到期序不变）。
	p.SetCreditPackages("u2", nil)
	if n := p.CountExpiring24h(""); n != 0 {
		t.Fatalf("CountExpiring24h after drain u2 = %d, want 0", n)
	}
	if m, _ := h.groupDefaultModel(&g); m != "cn:glm-5.2" {
		t.Fatalf("boost off (0 accounts): default = %q, want cn:glm-5.2", m)
	}
}

// mgJSON 构造一个 JSON 响应（模型目录探测的假上游用）。
func mgJSON(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestChatGroupDefaultModelByRate 组内模型按**生效积分倍率升序**使用：
// 限时免费（倍率 0）排最前，其次低倍率；同倍率保持配置书写顺序。这里把免费模型
// 故意写在配置最后，断言 model 缺省时选中的仍是它——出站 body 与 /models 的
// data[0] 都应是该免费模型。
func TestChatGroupDefaultModelByRate(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()

	// /v3/config：四个模型给出不同牌价，deepseek-v4.1-flash 命中「限时免费」优惠。
	const v3Config = `{"code":0,"data":{"models":[
		{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":131000,"credits":"x0.79"},
		{"id":"hy3","name":"Hy3","maxInputTokens":1000000,"maxOutputTokens":64000,"credits":"x0.11"},
		{"id":"glm-5.3","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":131000,"credits":"x0.11"},
		{"id":"deepseek-v4.1-flash","name":"DS-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.30"}
	],"modelPromotions":[
		{"enabled":true,"priority":1,"modelIds":["deepseek-v4.1-flash"],
		 "badge":{"label":"限时免费"},"discount":{"discountedCredits":"0x","factor":0}}
	]}}`

	var mu sync.Mutex
	var chatModels []string
	up := &upstream.Client{
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, "/v3/config"):
				return mgJSON(200, v3Config), nil
			case strings.HasSuffix(req.URL.Path, "/console/enterprises/personal/models"):
				// 企业端点空目录：FetchModels 允许单路失败，v3/config 为准。
				return mgJSON(200, `{"code":0,"data":{"models":[],"agents":[]}}`), nil
			}
			var peek struct {
				Model string `json:"model"`
			}
			if req.Body != nil {
				raw, _ := io.ReadAll(req.Body)
				_ = json.Unmarshal(raw, &peek)
			}
			mu.Lock()
			chatModels = append(chatModels, peek.Model)
			mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
	}
	// 先刷一次目录，填充倍率快照（groupModelPriority 读的就是这份 ModelRate）。
	if _, err := up.FetchModels(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}); err != nil {
		t.Fatalf("seed model rates: %v", err)
	}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	grp := modelgroup.Group{Name: "dev", Models: []string{
		"cn:glm-5.2", "cn:hy3", "cn:glm-5.3", "cn:deepseek-v4.1-flash",
	}}
	h := newGroupHandler(p, up, []modelgroup.Group{grp})

	// 1) 生效优先级：免费最前 → 同倍率（0.11）按书写顺序 hy3 先于 glm-5.3 → 高倍率垫底。
	want := []string{"cn:deepseek-v4.1-flash", "cn:hy3", "cn:glm-5.3", "cn:glm-5.2"}
	if got := h.groupModelPriority(&grp); !reflect.DeepEqual(got, want) {
		t.Fatalf("priority = %v, want %v", got, want)
	}

	// 2) model 缺省 → 出站 model 为免费模型（尽管它在配置里排最后）。
	if rec := postChat(h, "/dev/v1/chat/completions", `{"messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotModels := append([]string{}, chatModels...)
	mu.Unlock()
	if len(gotModels) != 1 || gotModels[0] != "deepseek-v4.1-flash" {
		t.Fatalf("outbound model = %v, want [deepseek-v4.1-flash] (free first)", gotModels)
	}

	// 3) /models 也按同一优先级输出，data[0] = 免费模型。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dev/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models: got %d, want 200", rec.Code)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("models decode: %v", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, modelgroup.BareOf(d.ID))
	}
	if wantIDs := []string{"deepseek-v4.1-flash", "hy3", "glm-5.3", "glm-5.2"}; !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("models order = %v, want %v", ids, wantIDs)
	}
}

// resetModelsCache 清空全局模型目录缓存（含负缓存）：fetchDynamicModels 是包级
// 单例状态，模型组测试触发的失败探测若残留 lastFail，会污染后续模型列表测试
// （与 handler_test.go 既有测试的清缓存手法一致）。
func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// TestModelsGrouped 组的模型列表：未知组 404；组不限模型 = 全量；钉了清单 =
// 只列组内模型且按组内顺序输出。
func TestModelsGrouped(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.2", "cn:deepseek-v4-flash"}},
		{Name: "all"},
	})

	if rec := postChat(h, "/nope/v1/models", ""); rec.Code == 0 {
		t.Fatal("unreachable")
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if rec := get("/nope/v1/models"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group models: got %d, want 404", rec.Code)
	}
	// 池空模型目录（fake upstream 无 /models 行为）→ 全量组输出空列表但结构合法。
	rec := get("/all/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("unrestricted group models: got %d, want 200", rec.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []any  `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Object != "list" || out.Data == nil {
		t.Fatalf("unexpected models payload: %s", rec.Body.String())
	}
}

// TestChatGroupEffortInjection 组条目配置思考强度（"模型|档位"）时，组路由对该
// 模型的请求强制注入/覆盖出站 body 的 reasoning_effort：
//  1. 默认模型选中带档位条目 → 注入 max；
//  2. 客户端点名该模型且自带 reasoning_effort:"low" → 覆盖为 max（组配置即基线）；
//  3. 组配置档位的 deepseek 系模型 → 出站 max（组配置优先于 thinking.go 的
//     deepseek 缺档回退 high——组注入在出站管线之前，已有档位时回退跳过）。
func TestChatGroupEffortInjection(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.3-flash|max", "cn:deepseek-v4-flash|max"}},
	})

	// 1) 默认模型（假上游无目录倍率 → 按书写顺序取 glm-5.3-flash|max）。
	if rec := postChat(h, "/dev/v1/chat/completions", `{"messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("default model: got %d, want 200", rec.Code)
	}
	// 2) 客户端点名 + 自带档位 → 覆盖。
	if rec := postChat(h, "/dev/v1/chat/completions",
		`{"model":"cn:glm-5.3-flash","reasoning_effort":"low","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("member model with own effort: got %d, want 200", rec.Code)
	}
	// 3) deepseek 系 + 组配置档位 → 组配置压过缺档回退。
	if rec := postChat(h, "/dev/v1/chat/completions",
		`{"model":"cn:deepseek-v4-flash","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("deepseek member: got %d, want 200", rec.Code)
	}

	_, models, efforts := up.snapshot()
	if len(models) != 3 || len(efforts) != 3 {
		t.Fatalf("outbound calls = %v, efforts = %v, want 3 each", models, efforts)
	}
	if models[0] != "glm-5.3-flash" || efforts[0] != "max" {
		t.Fatalf("default pick: model=%q effort=%q, want glm-5.3-flash/max", models[0], efforts[0])
	}
	if models[1] != "glm-5.3-flash" || efforts[1] != "max" {
		t.Fatalf("override: model=%q effort=%q, want glm-5.3-flash/max (client value overwritten)", models[1], efforts[1])
	}
	if models[2] != "deepseek-v4-flash" || efforts[2] != "max" {
		t.Fatalf("deepseek: model=%q effort=%q, want deepseek-v4-flash/max (group config beats high fallback)", models[2], efforts[2])
	}
}

// TestChatGroupNoEffortNoInjection 组条目未配置档位时绝不注入 reasoning_effort
//（非 deepseek 模型无缺档回退，出站 effort 应保持缺失——默认档由上游自决；
// 实测上游对不认档位词的模型会把思考关掉）。
func TestChatGroupNoEffortNoInjection(t *testing.T) {
	up := newRecordUpstream(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := newGroupHandler(p, up.up, []modelgroup.Group{
		{Name: "dev", Models: []string{"cn:glm-5.2"}},
	})

	if rec := postChat(h, "/dev/v1/chat/completions", `{"messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("default model: got %d, want 200", rec.Code)
	}
	_, models, efforts := up.snapshot()
	if len(models) != 1 || models[0] != "glm-5.2" {
		t.Fatalf("outbound model = %v, want [glm-5.2]", models)
	}
	if efforts[0] != "" {
		t.Fatalf("unconfigured entry must NOT inject effort, got %q", efforts[0])
	}
}
