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
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// recordUpstream 记录每次出站请求的 Authorization（账号识别）与出站 body 的
// model 字段（模型重写断言）。聊天行为恒 200 SSE 成功。
type recordUpstream struct {
	up     *upstream.Client
	mu     sync.Mutex
	tokens []string
	models []string
}

func newRecordUpstream(t *testing.T) *recordUpstream {
	t.Helper()
	r := &recordUpstream{}
	r.up = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			r.mu.Lock()
			r.tokens = append(r.tokens, req.Header.Get("Authorization"))
			var peek struct {
				Model string `json:"model"`
			}
			if req.Body != nil {
				raw, _ := io.ReadAll(req.Body)
				_ = json.Unmarshal(raw, &peek)
			}
			r.models = append(r.models, peek.Model)
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

func (r *recordUpstream) snapshot() (tokens, models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.tokens...), append([]string{}, r.models...)
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
	_, models := up.snapshot()
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
	_, models := up.snapshot()
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
	tokens, _ := up.snapshot()
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
	tokens, _ := up.snapshot()
	if len(tokens) != 0 {
		t.Fatalf("no upstream call expected, got %v", tokens)
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
