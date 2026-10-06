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
		"cn:glm-5.2":        "glm-5.2",
		"global:gpt-x":      "gpt-x",
		"glm-5.2":           "glm-5.2",
		"other:model:x":     "other:model:x", // 非 cn/global 前缀视为裸名
		"":                  "",
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
	// Replace 热替换：旧组消失、新组可见（面板保存路径）。
	r.Replace([]Group{{Name: "ops"}})
	if _, ok := r.Lookup("dev"); ok {
		t.Error("replaced registry should drop old groups")
	}
	if _, ok := r.Lookup("ops"); !ok {
		t.Error("replaced registry should expose new groups")
	}
}
