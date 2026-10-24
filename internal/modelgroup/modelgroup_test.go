package modelgroup

import "testing"

func TestValidate(t *testing.T) {
	ok := []Group{
		{Name: "dev", Models: []string{" cn:glm-5.2 ", ""}, Accounts: []string{" u1 "}},
		{Name: "chat-01", Models: nil, Accounts: nil},
		{Name: "a_b-c9"},
	}
	if err := Validate(ok); err != nil {
		t.Fatalf("valid groups rejected: %v", err)
	}
	// trim 就地规范化
	if ok[0].Models[0] != "cn:glm-5.2" || len(ok[0].Models) != 1 {
		t.Fatalf("models trim/drop-empty failed: %q", ok[0].Models)
	}
	if ok[0].Accounts[0] != "u1" {
		t.Fatalf("accounts trim failed: %q", ok[0].Accounts)
	}

	bad := []struct {
		name   string
		groups []Group
	}{
		{"empty name", []Group{{Name: " "}}},
		{"reserved v1", []Group{{Name: "v1"}}},
		{"reserved panel", []Group{{Name: "panel"}}},
		{"reserved chat", []Group{{Name: "chat"}}},
		{"uppercase", []Group{{Name: "Dev"}}},
		{"bad char", []Group{{Name: "de v"}}},
		{"leading dash", []Group{{Name: "-dev"}}},
		{"too long", []Group{{Name: string(make([]byte, 33))}}},
		{"dup", []Group{{Name: "dev"}, {Name: "dev"}}},
	}
	for _, tc := range bad {
		if err := Validate(tc.groups); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestBareOf(t *testing.T) {
	cases := map[string]string{
		"cn:glm-5.2":    "glm-5.2",
		"global:gpt-x":  "gpt-x",
		"glm-5.2":       "glm-5.2",
		"other:model:x": "other:model:x", // 非 cn/global 前缀视为裸名
		"":              "",
	}
	for in, want := range cases {
		if got := BareOf(in); got != want {
			t.Errorf("BareOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllows(t *testing.T) {
	g := Group{Name: "dev", Models: []string{"cn:glm-5.2", "global:gpt-x"}}
	if !g.Allows("glm-5.2") || !g.Allows("gpt-x") {
		t.Error("prefixed whitelist members should allow bare names")
	}
	if g.Allows("kimi") {
		t.Error("non-member should be rejected")
	}
	open := Group{Name: "all"}
	if !open.Allows("anything") {
		t.Error("empty whitelist = unrestricted")
	}
}

// TestSplitEffort 思考强度后缀剥离："模型|档位" → (模型, 档位)；无后缀/空档位
// 原样返回空档位。
func TestSplitEffort(t *testing.T) {
	cases := []struct {
		in     string
		model  string
		effort string
	}{
		{"cn:glm-5.3-flash|max", "cn:glm-5.3-flash", "max"},
		{"glm-5.2", "glm-5.2", ""},
		{"cn:glm-5.2|", "cn:glm-5.2", ""},             // 空档位 = 未配置
		{"cn:glm-5.2| xhigh ", "cn:glm-5.2", "xhigh"}, // 档位 trim
		{"cn:a|b|c", "cn:a|b", "c"},                   // 取最后一个 |
		{"", "", ""},
	}
	for _, tc := range cases {
		m, e := SplitEffort(tc.in)
		if m != tc.model || e != tc.effort {
			t.Errorf("SplitEffort(%q) = (%q, %q), want (%q, %q)", tc.in, m, e, tc.model, tc.effort)
		}
	}
}

// TestEffortOf 按裸名反查组内思考强度配置； Allows/EffortOf 均以后缀剥离后的
// 裸名判等。
func TestEffortOf(t *testing.T) {
	g := Group{Name: "dev", Models: []string{"cn:glm-5.3-flash|max", "cn:deepseek-v4-flash"}}
	if got := g.EffortOf("glm-5.3-flash"); got != "max" {
		t.Errorf("EffortOf(glm-5.3-flash) = %q, want max", got)
	}
	if got := g.EffortOf("deepseek-v4-flash"); got != "" {
		t.Errorf("EffortOf(unconfigured) = %q, want empty", got)
	}
	if got := g.EffortOf("kimi"); got != "" {
		t.Errorf("EffortOf(non-member) = %q, want empty", got)
	}
	// 白名单判等不受后缀影响。
	if !g.Allows("glm-5.3-flash") {
		t.Error("Allows should strip effort suffix before matching")
	}
}

// TestFitEffort 面板保存时的档位自动更正（用户请求的核心规则）：
//
//	|max 但模型没有 max → 最高档；|low 但模型没有 low → 最低档；
//	模型支持该档 / 未加注 / 能力未知 / 档位词不在强弱序 → 一律不动。
func TestFitEffort(t *testing.T) {
	cases := []struct {
		name      string
		effort    string
		supported []string
		want      string
		changed   bool
	}{
		{"支持则不变", "max", []string{"low", "high", "max"}, "max", false},
		{"max缺失→最高档", "max", []string{"low", "high"}, "high", true},
		{"low缺失→最低档", "low", []string{"high", "xhigh"}, "high", true},
		{"中间档缺失→降级到最近低档", "medium", []string{"low", "high"}, "low", true},
		{"低于最低档→最低档", "off", []string{"low", "high"}, "low", true},
		{"单档表→该档", "xhigh", []string{"medium"}, "medium", true},
		{"能力未知→不动", "max", nil, "max", false},
		{"能力空表→不动", "max", []string{}, "max", false},
		{"空档位→不动", "", []string{"low"}, "", false},
		{"未知档位词→不动（不臆造）", "turbo", []string{"low", "high"}, "turbo", false},
		{"大小写不敏感且取原文形态", "MAX", []string{"low", "Max"}, "Max", false},
		{"空白trim", " high ", []string{"low", "high"}, "high", false},
		{"off在表内→不动", "off", []string{"off", "low", "high"}, "off", false},
	}
	for _, tc := range cases {
		got, changed := FitEffort(tc.effort, tc.supported)
		if got != tc.want || changed != tc.changed {
			t.Errorf("%s: FitEffort(%q, %v) = (%q, %v), want (%q, %v)",
				tc.name, tc.effort, tc.supported, got, changed, tc.want, tc.changed)
		}
	}
}

// TestValidateEffortLexical 档位词法校验：小写字母/数字开头、仅小写字母数字-_、
// 1-16 长度；大写/空格/超长拒绝（档位表本身由上游裁决，此处只拦词法错误）。
func TestValidateEffortLexical(t *testing.T) {
	if err := Validate([]Group{{Name: "dev", Models: []string{"cn:m|max", "cn:n|xhigh"}}}); err != nil {
		t.Fatalf("valid efforts rejected: %v", err)
	}
	for _, bad := range []string{"cn:m|Max", "cn:m|hi gh", "cn:m|-low", "cn:m|" + string(make([]byte, 17))} {
		if err := Validate([]Group{{Name: "dev", Models: []string{bad}}}); err == nil {
			t.Errorf("effort %q: expected error, got nil", bad)
		}
	}
}

func TestRegistry(t *testing.T) {
	var nilReg *Registry
	if _, ok := nilReg.Lookup("dev"); ok {
		t.Error("nil registry should never match")
	}
	if nilReg.List() != nil {
		t.Error("nil registry List should be nil")
	}

	r := NewRegistry([]Group{{Name: "dev"}, {Name: "chat"}})
	if _, ok := r.Lookup("dev"); !ok {
		t.Error("dev should resolve")
	}
	if _, ok := r.Lookup("DEV"); ok {
		t.Error("lookup is case-sensitive (names are lowercase-only)")
	}
	if _, ok := r.Lookup("v1"); ok {
		t.Error("reserved name should never resolve even if injected")
	}
	if _, ok := r.Lookup("chat"); ok {
		t.Error("reserved name chat should never resolve even if injected")
	}
	// Replace 热替换：旧组消失、新组可见（面板保存路径）。
	r.Replace([]Group{{Name: "ops"}})
	if _, ok := r.Lookup("dev"); ok {
		t.Error("replaced registry should drop old groups")
	}
	if _, ok := r.Lookup("ops"); !ok {
		t.Error("replaced registry should expose new groups")
	}
}
