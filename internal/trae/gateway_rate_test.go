// ModelEntries 倍率透出：面板「积分倍率」列与 /v1/models 消费者都读 credits
// 字段（格式 "x0.06"）。倍率未知（Rate=0）必须省略，不编造假价格。
package trae

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
	traepool "github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
)

func TestFmtCreditsRate(t *testing.T) {
	cases := []struct {
		rate float64
		want string
	}{
		{0.06, "x0.06"},
		{0.78, "x0.78"},
		{1.83, "x1.83"},
		{1, "x1"},
		{0.5, "x0.5"},
	}
	for _, c := range cases {
		if got := fmtCreditsRate(c.rate); got != c.want {
			t.Errorf("fmtCreditsRate(%v) = %q, want %q", c.rate, got, c.want)
		}
	}
}

// 动态表拉取失败（此处指向不可达的假 host）时回退静态表：条目仍带 trae:
// 前缀可渲染，且不带任何倍率——上游倍率只来自动态表，失败不编造假价格。
func TestModelEntriesStaticFallbackNoCredits(t *testing.T) {
	now := time.Now()
	tp := traepool.New("")
	tp.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: now.Add(time.Hour).Unix()})
	uc := upstream.New()
	uc.AgentHost = "http://127.0.0.1:1"
	uc.HTTP = &http.Client{Timeout: 2 * time.Second}
	g := New(Config{Pool: tp, Upstream: uc, WorkMode: "disabled", CreditResetDay: 1})
	entries := g.ModelEntries()
	if len(entries) == 0 {
		t.Fatal("static fallback returned no entries")
	}
	for _, e := range entries {
		if _, ok := e["credits"]; ok {
			t.Fatalf("static fallback entry %v must not carry credits", e["id"])
		}
		if id, _ := e["id"].(string); !strings.HasPrefix(id, "trae:") {
			t.Fatalf("entry id missing trae: prefix: %v", e["id"])
		}
	}
}

// 静态表本身不含倍率：上游倍率只来自动态表（get_detail_param）。
func TestStaticModelsHaveNoRateField(t *testing.T) {
	for _, m := range staticModels {
		if _, ok := m["credits"]; ok {
			t.Fatalf("static model %v carries credits", m["id"])
		}
		if _, ok := m["rate"]; ok {
			t.Fatalf("static model %v carries rate", m["id"])
		}
	}
}
