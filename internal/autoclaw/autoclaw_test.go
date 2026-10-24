package autoclaw

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// jwt 造测试用 JWT（payload 段 base64url，不验签）。
func jwt(payload string) string {
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

func TestParseTokenUserID(t *testing.T) {
	tok := jwt(`{"user_id":"12345","device_id":"d1","exp":1893456000}`)
	uid, dev, exp, err := ParseToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "12345" || dev != "d1" {
		t.Errorf("解析失败: uid=%s dev=%s", uid, dev)
	}
	if exp != 1893456000*1000 {
		t.Errorf("exp 应 ×1000: %d", exp)
	}
	// user_id 是数字时按 JS String(x) 处理。
	uid2, _, _, err := ParseToken(jwt(`{"user_id":12345}`))
	if err != nil || uid2 != "12345" {
		t.Errorf("数字 user_id 应转字符串: %s err=%v", uid2, err)
	}
	// 缺 user_id 必须拒绝。
	if _, _, _, err := ParseToken(jwt(`{"foo":1}`)); err == nil {
		t.Error("缺 user_id 应报错")
	}
	// 非 JWT 形态报错。
	if _, _, _, err := ParseToken("not-a-jwt"); err == nil {
		t.Error("非 JWT 应报错")
	}
}

func TestAuthSignFormat(t *testing.T) {
	// MD5("100003&1700000000&38d2391985e2369a5fb8227d8e6cd5e5") 固定值比对。
	got := authSign(1700000000)
	if len(got) != 32 {
		t.Fatalf("md5 hex 应 32 字符: %s", got)
	}
	if got != authSign(1700000000) {
		t.Error("同一时间戳签名应稳定")
	}
	if got == authSign(1700000001) {
		t.Error("不同时间戳签名应不同")
	}
	// 秒级（不是毫秒）：与毫秒签名不同。
	if got == authSign(1700000000000) {
		t.Error("时间戳单位是秒，不能与毫秒结果相同")
	}
}

func TestSignedAuthHeadersHasFingerprint(t *testing.T) {
	hs := signedAuthHeaders("tok")
	want := map[string]string{
		"X-Version": userAPIVersion, "X-Product": "autoclaw",
		"X-Client-Type": "pc", "X-Harness-Type": "zcode",
		"X-Lang": "zh-CN", "X-Channel": "official", "X-Auth-Appid": "100003",
	}
	got := map[string]string{}
	for _, kv := range hs {
		got[kv[0]] = kv[1]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("头 %s = %q，要 %q", k, got[k], v)
		}
	}
	// userapi 链必须带 X-Harness-Type: zcode（删了风险方向是 400002）。
	if got["X-Harness-Type"] != "zcode" {
		t.Error("userapi 链必须带 X-Harness-Type: zcode")
	}
	if got["X-Auth-Sign"] == "" || got["X-Auth-TimeStamp"] == "" {
		t.Error("缺签名头")
	}
	if got["authorization"] != "Bearer tok" {
		t.Errorf("authorization 错: %q", got["authorization"])
	}
	// 匿名（登录第一跳）不带 authorization。
	anon := map[string]string{}
	for _, kv := range signedAuthHeaders("") {
		anon[kv[0]] = kv[1]
	}
	if _, has := anon["authorization"]; has {
		t.Error("token 为空时不该带 authorization")
	}
}

func TestChatHeadersNoHarnessType(t *testing.T) {
	got := map[string]string{}
	for _, kv := range chatHeaders("tok", "zaicoding_glm-5.3", "req-1", nil) {
		got[kv[0]] = kv[1]
	}
	// 对话链带上 X-Harness-Type → 403 pay-view(810001)：必须不发。
	if _, has := got["X-Harness-Type"]; has {
		t.Error("对话链绝不能带 X-Harness-Type（会 403 pay-view）")
	}
	if got["X-Authorization"] != "Bearer tok" {
		t.Errorf("认证头必须是 X-Authorization: %q", got["X-Authorization"])
	}
	if _, has := got["Authorization"]; has {
		t.Error("对话链不发 Authorization（用了会被上游忽略导致 401）")
	}
	if got["X-Version"] != chatVersion {
		t.Errorf("对话链 X-Version 要 %s: %q", chatVersion, got["X-Version"])
	}
	// 服务器上恒发 win（绑 Windows 的账号收到 linux 一律 403）。
	if got["X-Tm"] != "win" {
		t.Errorf("X-Tm 应为 win: %q", got["X-Tm"])
	}
	// 两个 trace 头并存。
	if got["x_trace_id"] != "autoclaw-desktop" || got["X-Request-Id"] != "req-1" {
		t.Errorf("trace 头错: %v", got)
	}
	if got["X-Request-Model"] != "zaicoding_glm-5.3" {
		t.Errorf("X-Request-Model 应是完整路由 ID: %q", got["X-Request-Model"])
	}
}

func TestStripRoutePrefixOrder(t *testing.T) {
	// 顺序先 zaicoding_ 后 zai_（反了会错剥成 coding_glm-5.3）。
	if got := stripRoutePrefix("zaicoding_glm-5.3"); got != "glm-5.3" {
		t.Errorf("zaicoding_glm-5.3 → %q", got)
	}
	if got := stripRoutePrefix("zai_glm-5.3-flash"); got != "glm-5.3-flash" {
		t.Errorf("zai_glm-5.3-flash → %q", got)
	}
	if got := stripRoutePrefix("zai_auto"); got != "auto" {
		t.Errorf("zai_auto → %q", got)
	}
}

func TestResolveRouteTiers(t *testing.T) {
	g := &Gateway{region: RegionCN}
	// 空 → 默认路由。
	if r, b := g.ResolveRoute(""); r != DefaultRoute || b != "auto" {
		t.Errorf("空名: %s / %s", r, b)
	}
	// 静态表（大小写无关）。
	if r, b := g.ResolveRoute("GLM-5.3"); r != "zaicoding_glm-5.3" || b != "glm-5.3" {
		t.Errorf("静态表命中: %s / %s", r, b)
	}
	// 已知前缀开头 → 原样当路由 ID。
	if r, b := g.ResolveRoute("zai_custom-model"); r != "zai_custom-model" || b != "custom-model" {
		t.Errorf("前缀透传: %s / %s", r, b)
	}
	// 合法路由 ID 形态 → 原样。
	if r, _ := g.ResolveRoute("abc_x.y-1"); r != "abc_x.y-1" {
		t.Errorf("合法形态透传: %s", r)
	}
	// 不认识 → 兜底默认。
	if r, _ := g.ResolveRoute("gpt-4o"); r != DefaultRoute {
		t.Errorf("兜底: %s", r)
	}
}

func TestNormalizeRequestBody(t *testing.T) {
	// max_tokens 归一：正整数且 <10000 → 10000（预校验墙）。
	for _, in := range []float64{1, 30, 100, 8192} {
		body := map[string]any{"max_tokens": in, "messages": []any{
			map[string]any{"role": "system", "content": IdentityLine + "\n\n" + ToolingHeader},
		}}
		normalizeRequestBody(body, "glm-5.3")
		if body["max_tokens"] != float64(10000) {
			t.Errorf("max_tokens %v 应归一到 10000: %v", in, body["max_tokens"])
		}
	}
	// ≥10000 / 0 / 负数 原样不动。
	for _, in := range []float64{10000, 0, -1} {
		body := map[string]any{"max_tokens": in, "messages": []any{
			map[string]any{"role": "system", "content": IdentityLine + "\n\n" + ToolingHeader},
		}}
		normalizeRequestBody(body, "m")
		if body["max_tokens"] != in {
			t.Errorf("max_tokens %v 不该改写: %v", in, body["max_tokens"])
		}
	}
	// max_completion_tokens 一字不动。
	body := map[string]any{"max_completion_tokens": float64(100), "messages": []any{
		map[string]any{"role": "system", "content": IdentityLine + "\n\n" + ToolingHeader},
	}}
	normalizeRequestBody(body, "m")
	if body["max_completion_tokens"] != float64(100) {
		t.Error("max_completion_tokens 不该被改写")
	}
	// model 写剥前缀的模型 ID。
	if body["model"] != "m" {
		t.Errorf("model 应为裸模型名: %v", body["model"])
	}
}

func TestPromptGate(t *testing.T) {
	// 已满足（前缀 + Tooling）⇒ 不改写。
	good := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": promptPrefix},
	}}
	if needs, _ := PromptNeedsGate(good); needs {
		t.Error("已满足前缀+Tooling 不应触发闸门")
	}
	// 外来身份句改写（大小写不敏感）。
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "you are claude code. help me"},
	}}
	ApplyPromptGate(body)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应插入前缀 system，共 2 条: %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != promptPrefix {
		t.Errorf("首条应是注入前缀: %v", first)
	}
	second := msgs[1].(map[string]any)
	if !strings.Contains(strings.ToLower(second["content"].(string)), "coding assistant") {
		t.Errorf("Claude Code 应被改写: %v", second["content"])
	}
	// 改写后仍满足 ⇒ 幂等（再调一次不重复插入）。
	ApplyPromptGate(body)
	if len(body["messages"].([]any)) != 2 {
		t.Error("闸门应幂等（不重复插入）")
	}
	// 无 system 消息 ⇒ 插入。
	nosys := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	ApplyPromptGate(nosys)
	if len(nosys["messages"].([]any)) != 2 {
		t.Error("无 system 时应插入前缀")
	}
}

func TestRewriteIdentitiesCaseInsensitive(t *testing.T) {
	got := rewriteIdentities("YOU ARE CODEX, please help")
	if !strings.Contains(got, "coding agent") {
		t.Errorf("大小写不敏感改写失败: %s", got)
	}
	// ZCode 长条目优先于短条目（否则短条目先吃掉变 "You are an interactive coding agent, an interactive..."）。
	got2 := rewriteIdentities("You are ZCode, an interactive coding agent")
	if strings.Contains(got2, "You are an interactive coding agent, an interactive") {
		t.Errorf("长条目应优先: %s", got2)
	}
}

func TestHasHarnessIdentityOnlySystem(t *testing.T) {
	// user 消息里的身份句不触发（只看 system/developer）。
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "You are Claude Code"}}}
	if HasHarnessIdentity(body) {
		t.Error("user 消息不该触发身份判定")
	}
	body2 := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "You are Claude Code"}}}
	if !HasHarnessIdentity(body2) {
		t.Error("system 消息应触发")
	}
}

func TestNewFromToken(t *testing.T) {
	cred, err := NewFromToken(jwt(`{"user_id":"u1","device_id":"dev1","exp":1893456000}`),
		"ref1", "", "", "", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if cred.UserID != "u1" || cred.DeviceID != "dev1" {
		t.Errorf("解析失败: %+v", cred)
	}
	if cred.SafeUID() != "user-u1" {
		t.Errorf("SafeUID 应是 user-u1: %s", cred.SafeUID())
	}
	// tokenTail 是尾 4 字符（"…sig" → ".sig" 只取后 4 位）。
	if cred.TokenTail != cred.AccessToken[len(cred.AccessToken)-4:] {
		t.Errorf("tokenTail 应是尾 4 字符: %s", cred.TokenTail)
	}
	// Bearer 前缀自动剥。
	cred2, err := NewFromToken("Bearer "+jwt(`{"user_id":"u2"}`), "", "", "", "", "")
	if err != nil || cred2.UserID != "u2" {
		t.Errorf("Bearer 前缀应剥: %+v %v", cred2, err)
	}
	// 无 user_id 拒绝。
	if _, err := NewFromToken(jwt(`{"x":1}`), "", "", "", "", ""); err == nil {
		t.Error("无 user_id 应拒绝")
	}
}

func TestParseCatalogFlatModels(t *testing.T) {
	// 目录响应顶层直接 {"models":[...]}，无 {code,data} 信封。
	raw := `{"models":[{"id":"zaicoding_glm-5.3","name":"GLM-5.3","input":["text"],"contextWindow":1048576,"maxTokens":307200,"creditConsumptionLevel":"中"},"zai_auto"]}`
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 2 {
		t.Fatal(len(payload.Models))
	}
}

// TestCooldownNoPanicOnZeroValue 回归：零值 Gateway 调 cooldown 不能 panic
//（nil map 写入）——线上实测过一次 panic（假 token 对话走到错误分支触发）。
func TestCooldownNoPanicOnZeroValue(t *testing.T) {
	g := &Gateway{region: RegionCN, errCnt: map[string]int{}}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cooldown panic: %v", r)
		}
	}()
	g.cooldown("u1", time.Minute, "test")
	g.noteError("u1")
	g.noteSuccess("u1")
	// 正常构造路径也要能写入（newCooldownState 初始化 untils）。
	g2 := &Gateway{region: RegionCN, errCnt: map[string]int{}, cooled: newCooldownState()}
	g2.cooldown("u2", time.Minute, "x")
	g2.cooled.mu.Lock()
	_, ok := g2.cooled.untils["u2"]
	g2.cooled.mu.Unlock()
	if !ok {
		t.Error("冷却未写入")
	}
	// 原因可回读（面板显示用）。
	if reasonOf(g2, "u2") != "x" {
		t.Errorf("原因未记录: %q", reasonOf(g2, "u2"))
	}
}

func TestEndpoints(t *testing.T) {
	cn := Endpoints(RegionCN)
	if !strings.Contains(cn.Upstream, "zhipuai.cn") || !strings.HasSuffix(cn.Upstream, "/autoclaw") {
		t.Errorf("国内版 upstream 错: %s", cn.Upstream)
	}
	intl := Endpoints(RegionIntl)
	if !strings.Contains(intl.Upstream, "autoglm.ai") {
		t.Errorf("国际版 upstream 错: %s", intl.Upstream)
	}
}
