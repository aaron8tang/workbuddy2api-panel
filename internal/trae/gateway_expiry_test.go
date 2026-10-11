// gateway 到期估算：跨渠道「谁先到期用谁」排序的 trae 侧信号源。
// nextMonthlyReset 纯函数（本地时区月度重置日）+ EarliestCreditExpiry 门控
//（Servable=false / 重置日关闭 → ok=false）。
package trae

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	traepool "github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
)

func TestNextMonthlyReset(t *testing.T) {
	loc := time.Local
	cases := []struct {
		name string
		now  time.Time
		day  int
		want time.Time
	}{
		{"月中未来重置日", time.Date(2026, 10, 15, 12, 0, 0, 0, loc), 1, time.Date(2026, 11, 1, 0, 0, 0, 0, loc)},
		{"重置日当天已过零点", time.Date(2026, 10, 1, 8, 0, 0, 0, loc), 1, time.Date(2026, 11, 1, 0, 0, 0, 0, loc)},
		{"31日钳到当月月末", time.Date(2026, 2, 20, 0, 0, 0, 0, loc), 31, time.Date(2026, 2, 28, 0, 0, 0, 0, loc)},
		{"重置日在当月内", time.Date(2026, 1, 5, 0, 0, 0, 0, loc), 28, time.Date(2026, 1, 28, 0, 0, 0, 0, loc)},
		{"跨年12月→1月", time.Date(2026, 12, 15, 0, 0, 0, 0, loc), 1, time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextMonthlyReset(c.now, c.day)
			if !got.Equal(c.want) {
				t.Fatalf("nextMonthlyReset(%v, %d) = %v, want %v", c.now, c.day, got, c.want)
			}
		})
	}
}

func TestEarliestCreditExpiryGating(t *testing.T) {
	now := time.Now()

	// 有健康账号 + 重置日 → 下一次重置时刻。
	tp := traepool.New("")
	tp.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: now.Add(time.Hour).Unix()})
	g := New(Config{Pool: tp, Upstream: nil, WorkMode: "disabled", CreditResetDay: 1})
	got, ok := g.EarliestCreditExpiry()
	if !ok || !got.Equal(nextMonthlyReset(now, 1)) {
		t.Fatalf("fallback estimate: got %v ok=%v, want next reset", got, ok)
	}

	// 真实明细（SetCreditExpiry）优先于估算：取健康账号中最早未来到期时刻。
	real := now.Add(3 * 24 * time.Hour).Truncate(time.Second).Unix()
	tp.SetCreditExpiry("u1", real)
	got, ok = g.EarliestCreditExpiry()
	if !ok || got.Unix() != real {
		t.Fatalf("real detail: got %v ok=%v, want %v", got, ok, time.Unix(real, 0))
	}
	// 真实明细已过期 → 回退估算。
	tp.SetCreditExpiry("u1", now.Add(-time.Hour).Unix())
	got, ok = g.EarliestCreditExpiry()
	if !ok || !got.Equal(nextMonthlyReset(now, 1)) {
		t.Fatalf("expired detail fallback: got %v ok=%v, want next reset", got, ok)
	}
	tp.SetCreditExpiry("u1", 0)

	// 重置日关闭（0）且无明细 → 未知。
	g0 := New(Config{Pool: tp, Upstream: nil, WorkMode: "disabled", CreditResetDay: 0})
	if _, ok := g0.EarliestCreditExpiry(); ok {
		t.Fatal("credit_reset_day=0 且无明细应返回 ok=false")
	}

	// 无账号（不可服务）→ 未知。
	gEmpty := New(Config{Pool: traepool.New(""), Upstream: nil, WorkMode: "disabled", CreditResetDay: 1})
	if _, ok := gEmpty.EarliestCreditExpiry(); ok {
		t.Fatal("无可服务账号应返回 ok=false")
	}

	// 账号被冷却（不可服务）且无明细 → 未知。
	tp.Cooldown("u1", traepool.CoolPlan, time.Hour, "test")
	if _, ok := g.EarliestCreditExpiry(); ok {
		t.Fatal("账号全冷却且无明细应返回 ok=false")
	}

	// 冷却账号有明细但健康账号没有 → 冷却账号的到期不参与，回落估算。
	// （需要存在健康账号：全冷却时 Servable=false，估算兜底被门控，见上一段。）
	tp.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: now.Add(time.Hour).Unix()})
	got, ok = g.EarliestCreditExpiry()
	if !ok || !got.Equal(nextMonthlyReset(now, 1)) {
		t.Fatalf("cooling account detail excluded: got %v ok=%v, want next reset", got, ok)
	}
}
