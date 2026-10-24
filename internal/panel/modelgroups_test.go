package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelgroup"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSupportedEffortsFor 档位支持表查询：cn/global 走 Upstream 快照 ∪ 产品静态表，
// 未知模型/无静态表的渠道族（qoder 等）返回空 = 能力不可知。
func TestSupportedEffortsFor(t *testing.T) {
	p := &Panel{cfg: Config{Upstream: &upstream.Client{}}}
	cases := []struct {
		entry string
		want  []string
	}{
		{"cn:glm-5.2", []string{"high", "xhigh"}},         // 产品静态表
		{"glm-5.3-flash", []string{"low", "high", "max"}}, // 无前缀按 cn
		{"global:hy3", []string{"low", "high"}},           // global 静态表
		{"cn:nope", nil},                                  // 未知模型 → 不可知
		{"qoder:Qwen3.8-Flash", nil},                      // 该族无档位表 → 不可知
		{"trae:glm-5.2", nil},                             // 不可拿 CN 表跨渠道套
	}
	for _, tc := range cases {
		got := p.supportedEffortsFor(tc.entry)
		if !equalStrings(got, tc.want) {
			t.Errorf("supportedEffortsFor(%q) = %v, want %v", tc.entry, got, tc.want)
		}
	}
	if (&Panel{}).supportedEffortsFor("cn:glm-5.2") != nil {
		t.Error("Upstream 未装配时必须返回空（能力不可知）")
	}
}

// TestFitGroupEfforts 端到端：保存时 |max 无 max → 最高档、|low 无 low → 最低档，
// 档位已支持/未加注/能力不可知 → 原样保留。
func TestFitGroupEfforts(t *testing.T) {
	p := &Panel{cfg: Config{Upstream: &upstream.Client{}}}
	groups := []modelgroup.Group{{
		Name: "dev",
		Models: []string{
			"cn:glm-5.2|max",          // 支持 high/xhigh → xhigh
			"cn:glm-5.3-flash|max",    // 支持 low/high/max → 不变
			"cn:hy3|medium",           // 支持 low/high → low
			"cn:glm-5.2",              // 未加注 → 不变
			"cn:nope|max",             // 能力不可知 → 不变
			"qoder:Qwen3.8-Flash|max", // 该族无静态表 → 不变
			"cn:glm-5.2|xhigh",        // 已支持 → 不变
		},
	}}
	if err := modelgroup.Validate(groups); err != nil {
		t.Fatalf("validate: %v", err)
	}
	fixes := p.fitGroupEfforts(groups)
	want := []string{
		"cn:glm-5.2|xhigh",
		"cn:glm-5.3-flash|max",
		"cn:hy3|low",
		"cn:glm-5.2",
		"cn:nope|max",
		"qoder:Qwen3.8-Flash|max",
		"cn:glm-5.2|xhigh",
	}
	if !equalStrings(groups[0].Models, want) {
		t.Errorf("models after fit = %v, want %v", groups[0].Models, want)
	}
	// 更正记录：两条，且带可读原因。
	if len(fixes) != 2 {
		t.Fatalf("fixes = %d, want 2 (%+v)", len(fixes), fixes)
	}
	if fixes[0].From != "cn:glm-5.2|max" || fixes[0].To != "cn:glm-5.2|xhigh" || fixes[0].Reason == "" {
		t.Errorf("fix[0] = %+v", fixes[0])
	}
	if fixes[0].Group != "dev" {
		t.Errorf("fix[0].Group = %q, want dev", fixes[0].Group)
	}
	if fixes[1].From != "cn:hy3|medium" || fixes[1].To != "cn:hy3|low" {
		t.Errorf("fix[1] = %+v", fixes[1])
	}
}

// TestFixGroupEffortsKeepsUnknownWords 档位词不在已知强弱序 → 不臆造，原样保留。
func TestFixGroupEffortsKeepsUnknownWords(t *testing.T) {
	p := &Panel{cfg: Config{Upstream: &upstream.Client{}}}
	groups := []modelgroup.Group{{Name: "dev", Models: []string{"cn:glm-5.2|turbo"}}}
	fixes := p.fitGroupEfforts(groups)
	if len(fixes) != 0 || groups[0].Models[0] != "cn:glm-5.2|turbo" {
		t.Errorf("unknown effort must be kept: models=%v fixes=%+v", groups[0].Models, fixes)
	}
}

func TestSplitRealm(t *testing.T) {
	cases := []struct{ in, realm, bare string }{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:hy3", "global", "hy3"},
		{"trae:glm-5.2", "trae", "glm-5.2"},
		{"qoderwork:Qwen3.7-Max", "qoderwork", "Qwen3.7-Max"},
		{"glm-5.2", "cn", "glm-5.2"},
		{"weird:x", "cn", "weird:x"},
	}
	for _, tc := range cases {
		realm, bare := splitRealm(tc.in)
		if realm != tc.realm || bare != tc.bare {
			t.Errorf("splitRealm(%q) = (%q, %q), want (%q, %q)", tc.in, realm, bare, tc.realm, tc.bare)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
