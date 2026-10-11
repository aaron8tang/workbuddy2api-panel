package trae

import (
	"net/url"
	"strings"
	"testing"
)

// 构造一个合法回调 URL（query 已 percent-encode，复刻浏览器真实回调形态）。
func mustBuildCallback(t *testing.T, refreshToken, userInfoJSON, userJwtJSON string) string {
	t.Helper()
	v := url.Values{}
	v.Set("refreshToken", refreshToken)
	if userInfoJSON != "" {
		v.Set("userInfo", userInfoJSON)
	}
	if userJwtJSON != "" {
		v.Set("userJwt", userJwtJSON)
	}
	return "http://127.0.0.1:18080/authorize?" + v.Encode()
}

func TestParseCallbackWithRefreshToken(t *testing.T) {
	userInfo := `{"UserID":"u123","ScreenName":"Alice","TenantID":"ent-1"}`
	userJwt := `{"Token":"at-xyz","RefreshToken":"rt-fallback","TokenExpireAt":1786847930141}`
	cb := mustBuildCallback(t, "rt-main", userInfo, userJwt)

	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.RefreshToken != "rt-main" {
		t.Errorf("refreshToken=%q want rt-main", info.RefreshToken)
	}
	if info.AccessToken != "" {
		t.Errorf("accessToken should be empty when refreshToken present, got %q", info.AccessToken)
	}
	if info.UID != "u123" || info.Nickname != "Alice" || info.EnterpriseID != "ent-1" {
		t.Errorf("userInfo fields: %+v", info)
	}
}

func TestParseCallbackFallbackToUserJwt(t *testing.T) {
	// 回调缺 refreshToken → 回退 userJwt.RefreshToken 用于 ExchangeToken；
	// 此时 AccessToken 留空（由后续 ExchangeToken 填充），符合 login.sh 兜底分支语义。
	userInfo := `{"UserID":"u9","ScreenName":"Bob"}`
	userJwt := `{"Token":"at-from-jwt","RefreshToken":"rt-from-jwt","TokenExpireAt":1786847930141}`
	cb := mustBuildCallback(t, "", userInfo, userJwt)

	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// refreshToken 回退到 userJwt.RefreshToken，留给上游 ExchangeToken 使用
	if info.RefreshToken != "rt-from-jwt" {
		t.Errorf("refreshToken fallback=%q want rt-from-jwt", info.RefreshToken)
	}
	// 有可换的 refreshToken 时，AccessToken 不在此处填充（ExchangeToken 后才有）
	if info.AccessToken != "" {
		t.Errorf("accessToken should stay empty pre-ExchangeToken, got %q", info.AccessToken)
	}
	if info.ExpiresAt != 0 {
		t.Errorf("expiresAt should be 0 pre-ExchangeToken (got %d)", info.ExpiresAt)
	}
}

func TestParseCallbackNoRefreshButHasJwtToken(t *testing.T) {
	// 既无 query.refreshToken，userJwt 也无 RefreshToken，但有 Token → 用 Token 当 accessToken
	userJwt := `{"Token":"at-direct","TokenExpireAt":1786847930141}`
	cb := mustBuildCallback(t, "", `{"UserID":"u1","ScreenName":"S"}`, userJwt)
	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.AccessToken != "at-direct" {
		t.Errorf("accessToken=%q want at-direct", info.AccessToken)
	}
	if info.ExpiresAt != 1786847930 {
		t.Errorf("expiresAt=%d want 1786847930", info.ExpiresAt)
	}
}

func TestParseCallbackSingleEncodedUserInfo(t *testing.T) {
	// 真实回调：userInfo 为单层 percent-encoded JSON；url.Query() 已解一层，
	// parseJSONParam 第一个 candidate 即原始 JSON，直接命中。
	userInfo := `{"UserID":"ud","ScreenName":"Dan","TenantID":"t"}`
	cb := mustBuildCallback(t, "rt", userInfo, "")
	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.UID != "ud" || info.Nickname != "Dan" || info.EnterpriseID != "t" {
		t.Errorf("userInfo not recovered: %+v", info)
	}
}

func TestParseCallbackMissingAllTokens(t *testing.T) {
	userInfo := `{"UserID":"u1","ScreenName":"N"}`
	// 无 refreshToken，userJwt 也没有 Token
	cb := mustBuildCallback(t, "", userInfo, `{"RefreshToken":""}`)
	if _, err := ParseCallback(cb); err == nil {
		t.Fatal("want error when both refreshToken and userJwt.Token missing")
	}
}

func TestParseCallbackEmpty(t *testing.T) {
	if _, err := ParseCallback(""); err == nil {
		t.Fatal("want error for empty url")
	}
	if _, err := ParseCallback("   "); err == nil {
		t.Fatal("want error for blank url")
	}
}

func TestParseCallbackGarbledUserInfo(t *testing.T) {
	// userInfo 非 JSON → 字段为零但不报错（仅 token 必须有）
	cb := mustBuildCallback(t, "rt", "not-a-json", "")
	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatalf("garbled userInfo should not error: %v", err)
	}
	if info.UID != "" || info.Nickname != "" {
		t.Errorf("garbled userInfo should yield empty fields: %+v", info)
	}
	if info.RefreshToken != "rt" {
		t.Errorf("refreshToken lost: %+v", info)
	}
}

// TestNormalizeExpire 验证 TokenExpireAt 毫秒 → Unix 秒归一化。
// 注：原 trae2api 的 ExpireAtFromExchange(expireAt, duration, now) 中
// 「过期回退 now+duration」逻辑在合并项目中内联于 upstream 的 ExchangeToken
// （无独立可测函数），故该子用例不移植；此处仅测 normalizeExpire 的归一化语义。
func TestNormalizeExpire(t *testing.T) {
	// TokenExpireAt 毫秒（>1e12）→ 归一化为秒
	if got := normalizeExpire(1786847930141); got != 1786847930 {
		t.Errorf("millis: got %d want 1786847930", got)
	}
	// 秒级时间戳 → 原样返回
	if got := normalizeExpire(1786847930); got != 1786847930 {
		t.Errorf("seconds passthrough: got %d want 1786847930", got)
	}
	// 缺失 → 0
	if normalizeExpire(0) != 0 {
		t.Error("zero input should return 0")
	}
}

func TestBuildLoginURL(t *testing.T) {
	machineID := "abcdef0123456789abcdef0123456789"
	deviceID := "0123456789abcdef0123456789abcdef"
	cb := "http://127.0.0.1:7864/authorize"

	u := BuildLoginURL(machineID, deviceID, cb)
	if !strings.HasPrefix(u, "https://www.trae.cn/authorization?") {
		t.Fatalf("url prefix wrong: %s", u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse built url: %v", err)
	}
	q := parsed.Query()
	// 核心参数齐全
	checks := map[string]string{
		"client_id":         "en1oxy7wnw8j9n",
		"auth_from":         "solo",
		"login_channel":     "native_ide",
		"auth_type":         "local",
		"machine_id":        machineID,
		"device_id":         deviceID,
		"x_machine_id":      machineID,
		"x_device_id":       deviceID,
		"x_device_brand":    "PC",
		"x_device_type":     "PC",
		"auth_callback_url": cb,
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("param %s=%q want %q", k, got, want)
		}
	}
	// login_trace_id 存在且为 hex16
	if tid := q.Get("login_trace_id"); len(tid) != loginTraceIDHexLen {
		t.Errorf("login_trace_id len=%d want %d (val=%s)", len(tid), loginTraceIDHexLen, tid)
	}
}
