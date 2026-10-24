package qoder

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

func TestUnixSeconds(t *testing.T) {
	// 毫秒时间戳必须折算秒——读错会把过期推到数万年后
	ms := int64(1792000000000)
	got := unixSeconds(ms)
	want := time.Unix(1792000000, 0)
	if !got.Equal(want) {
		t.Errorf("unixSeconds(%d)=%v want %v", ms, got, want)
	}
}

func TestParseExpiry(t *testing.T) {
	now := time.Unix(1792000000, 0)
	cases := []struct {
		abs    string
		in     int64
		equal  time.Time
	}{
		{"2026-10-14T23:14:55Z", 0, time.Date(2026, 10, 14, 23, 14, 55, 0, time.UTC)},
		{"", 3600, now.Add(time.Hour)},
		{"1792003600", 0, now.Add(time.Hour)},
		{"garbage", 0, time.Time{}},
	}
	for i, c := range cases {
		if got := parseExpiry(c.abs, c.in, now); !got.Equal(c.equal) {
			t.Errorf("case %d: parseExpiry(%q,%d)=%v want %v", i, c.abs, c.in, got, c.equal)
		}
	}
}

func TestPkcePair(t *testing.T) {
	v, ch, err := pkcePair()
	if err != nil {
		t.Fatalf("pkcePair: %v", err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length=%d", len(v))
	}
	for _, r := range v {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~", r) {
			t.Errorf("verifier charset violation: %q", v)
			break
		}
	}
	if len(ch) != 43 {
		t.Errorf("challenge length=%d", len(ch))
	}
}

func TestConversationSessionID(t *testing.T) {
	a := conversationSessionID("uid1", "conv1")
	b := conversationSessionID("uid1", "conv1")
	c := conversationSessionID("uid1", "conv2")
	if a != b {
		t.Error("same (uid,conv) must derive stable session id")
	}
	if a == c {
		t.Error("different conv must derive different session id")
	}
	// 36 位 UUID 形态校验（version 位按 API-Console 参考实现 |=0x80，非标准 v4）
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Errorf("not uuid-shaped: %q", a)
	}
}

func TestClassifyQoderSentinels(t *testing.T) {
	err := classifyQoder(403, []byte(`upgrade at https://qoder.com/pricing to continue`))
	if !errors.Is(err, ErrNoEntitlement) {
		t.Errorf("pricing body must map to ErrNoEntitlement, got %v", err)
	}
	var ue *cosy.Error
	if errors.As(err, &ue) && ue.Kind == cosy.ErrSessionDead {
		t.Errorf("no-entitlement must NOT be session-dead (would disable account): %v", err)
	}

	err = classifyQoder(403, []byte(`{"message":"billing daily count exceeded"}`))
	if !errors.Is(err, ErrDailyCountExceeded) {
		t.Errorf("daily count must map to ErrDailyCountExceeded, got %v", err)
	}

	err = classifyQoder(403, []byte(`{"error":{"code":10605,"message":"shared queue"}}`))
	var e2 *cosy.Error
	if !errors.As(err, &e2) || e2.Kind != cosy.ErrBusy {
		t.Errorf("10605 must map to ErrBusy, got %v", err)
	}

	// 普通认证失败走通用分类：401+TOKEN_EXPIRE → TokenExpired（refresh 可修复）
	err = classifyQoder(401, []byte(`{"message":"TOKEN_EXPIRED"}`))
	if !errors.As(err, &e2) || e2.Kind != cosy.ErrTokenExpired {
		t.Errorf("401 token expired must be TokenExpired, got %v", err)
	}

	// 403+「Signature invalid」（实测 18:31:42 帧形态）：签名抖动是 EnsureDT
	// 可修复的临时态，必须 TokenExpired 而非 SessionDead（后者会 Disable 账号）。
	err = classifyQoder(403, []byte(`{"code":"101","message":"Signature invalid"}`))
	if !errors.As(err, &e2) || e2.Kind != cosy.ErrTokenExpired {
		t.Errorf("signature invalid must be TokenExpired (refresh-fixable), got %v", err)
	}
}

func TestBuildMessagesFoldsSystemAndDropsOrphanTool(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "be terse"},
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call1", "type": "function",
				"function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
		}},
		// 悬空 tool 结果（call2 无人发起）——必须剔除
		{"role": "tool", "tool_call_id": "call2", "content": "orphan"},
		// 有效 tool 结果（call1）——保留
		{"role": "tool", "tool_call_id": "call1", "content": "42"},
	}
	out, systemText := buildMessages(msgs)
	if systemText != "be terse" {
		t.Errorf("systemText=%q", systemText)
	}
	if out[0].Role != "system" || out[0].Content != "be terse" {
		t.Errorf("first message=%+v", out[0])
	}
	roles := make([]string, 0, len(out))
	for _, m := range out {
		roles = append(roles, m.Role)
	}
	joined := strings.Join(roles, ",")
	if strings.Contains(joined, "tool,tool") || strings.Count(joined, "tool") != 1 {
		t.Errorf("orphan tool result must be dropped; roles=%v", roles)
	}
}

func TestBuildChatBodyDefaults(t *testing.T) {
	raw, err := BuildChatBody(ChatOptions{
		Messages:    []map[string]any{{"role": "user", "content": "hi"}},
		Model:       modelEntry{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", IsReasoning: true},
		RequestID:   "req1",
		RequestSetID: "set1",
		SessionID:   "sess1",
		ClientVersion: "1.0.45",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// tools 恒为数组（非 null）
	tools, ok := m["tools"].([]any)
	if !ok || tools == nil {
		t.Errorf("tools must be [] (never null), got %v", m["tools"])
	}
	// 未传 effort → 不开思考
	params, ok := m["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing: %v", m["parameters"])
	}
	if params["enable_thinking"] != nil {
		t.Errorf("enable_thinking must be absent without effort: %v", params["enable_thinking"])
	}
	if params["max_tokens"].(float64) != 32000 {
		t.Errorf("default max_tokens=%v", params["max_tokens"])
	}
	// session_type（qoder_work）+ business.sub_task（ws_builtin_general）
	if m["session_type"] != "qoder_work" {
		t.Errorf("session_type=%v", m["session_type"])
	}
	if biz, ok := m["business"].(map[string]any); !ok || biz["sub_task"] != subTask {
		t.Errorf("business.sub_task=%v", m["business"])
	}
	// effort=xhigh → 打开思考
	raw, err = BuildChatBody(ChatOptions{
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Model:    modelEntry{Key: "k"}, RequestID: "r", RequestSetID: "s", SessionID: "ss",
		ReasoningEffort: "xhigh",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &m)
	params = m["parameters"].(map[string]any)
	if params["enable_thinking"] != true || params["reasoning_effort"] != "xhigh" {
		t.Errorf("params=%v", params)
	}
	// effort=none → 显式关闭
	raw, _ = BuildChatBody(ChatOptions{
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Model:    modelEntry{Key: "k"}, RequestID: "r", RequestSetID: "s", SessionID: "ss",
		ReasoningEffort: "none",
	})
	_ = json.Unmarshal(raw, &m)
	params = m["parameters"].(map[string]any)
	if params["enable_thinking"] != false {
		t.Errorf("effort=none must disable thinking, got %v", params["enable_thinking"])
	}
}

func TestCredSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := &Cred{
		UID: "u1", Name: "nick", Email: "e@x.com", OrgID: "org1",
		OrgTags: []string{"t1"}, MachineID: "mid-1",
		DT: "dt-x", DRT: "drt-y",
		DTExpiry: time.Unix(1900000000, 0), DRTExpiry: time.Unix(2200000000, 0),
		RuntimeKey: "rk", RuntimeInfo: "ri",
	}
	if err := c.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadFile(c.FilePath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.UID != "u1" || got.MachineID != "mid-1" || got.RuntimeKey != "rk" || got.RuntimeInfo != "ri" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.AccessValid(time.Now()) == false {
		t.Error("loaded token should be valid until expiry")
	}
}

func TestNormalizeModelNameQoder(t *testing.T) {
	if got := normalizeModelName("Qwen3.7-Max"); got != "qwen3.7-max" {
		t.Errorf("got %q", got)
	}
}
