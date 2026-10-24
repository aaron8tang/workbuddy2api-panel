// EarliestCreditExpiry：模型组跨渠道「谁先到期用谁」排序的 workbuddy 侧信号源。
// 口径：healthy 账号 + 未来到期批次（remaining>0）；realm 过滤；冷却/禁用/过期
// 批次一律不参与，无有效信号返回 ok=false。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestEarliestCreditExpiry(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	now := time.Now()
	soon := now.Add(2 * time.Hour).Truncate(time.Second)
	far := now.Add(30 * 24 * time.Hour).Truncate(time.Second)

	mk := func(uid, realm string) {
		a := &auth.Auth{UID: uid}
		if realm != "cn" {
			if _, err := auth.BackfillRealmFor(a, realm); err != nil {
				t.Fatalf("BackfillRealmFor(%s): %v", realm, err)
			}
		}
		p.Add(a)
	}
	mk("cn-soon", "cn")
	mk("cn-far", "cn")
	mk("cn-expired", "cn")
	mk("cn-cooling", "cn")
	mk("gl-far", "global")
	// cn-soon / cn-far / gl-far：未来批次；cn-expired：批次已过期（应被忽略）；
	// cn-cooling：有批次但被冷却（应被忽略）。
	p.SetCreditsDetailed("cn-soon", 100, 0, 0, soon, 100)
	p.SetCreditsDetailed("cn-far", 100, 0, 0, far, 50)
	p.SetCreditsDetailed("cn-expired", 100, 0, 0, now.Add(-time.Hour), 100)
	p.SetCreditsDetailed("cn-cooling", 100, 0, 0, soon, 100)
	p.SetCreditsDetailed("gl-far", 100, 0, 0, far, 100)
	p.Cooldown("cn-cooling", CoolSoft, time.Hour, "test")

	// cn：最早未来批次来自 cn-soon（cn-expired 过期、cn-cooling 冷却均不参与）。
	got, ok := p.EarliestCreditExpiry("cn")
	if !ok || !got.Equal(soon) {
		t.Fatalf("cn = %v ok=%v, want %v ok=true", got, ok, soon)
	}
	// global：只有 gl-far。
	got, ok = p.EarliestCreditExpiry("global")
	if !ok || !got.Equal(far) {
		t.Fatalf("global = %v ok=%v, want %v ok=true", got, ok, far)
	}

	// 停用最早批次的账号后，信号回落到次早批次（cn-far）。
	p.Disable("cn-soon", "test")
	got, ok = p.EarliestCreditExpiry("cn")
	if !ok || !got.Equal(far) {
		t.Fatalf("cn after disable = %v ok=%v, want %v ok=true", got, ok, far)
	}

	// 无任何未来批次 → ok=false。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetCredits("u1", 100, 0)
	if _, ok := p2.EarliestCreditExpiry("cn"); ok {
		t.Fatal("无未来到期批次时应返回 ok=false")
	}
}

// TestEarliestCreditExpiryPkgsSignal 包明细信号（回归 2026-10-18 实案）：聚合快照
// 三字段全缺（balance 刷新被 billing 域 WAF 401 拦截的持续形态）但 pkgs 明细里
// 有 24h 内到期批次时，跨渠道「谁先到期用谁」必须照样生效——明细是独立于余额
// 刷新的数据源（每日拉取 + 每调用 FEFO 实时扣减），billing 域被拦不能让 CN 信号
// 恒 ok=false 而被 trae 渠道压过。
func TestEarliestCreditExpiryPkgsSignal(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "cn-pkgs"})
	// 关键场景：SetCreditsDetailed 从未写入 earliest（零值），只有 pkgs 明细。
	soon := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	p.SetCreditPackages("cn-pkgs", []PkgSnapshot{{Remain: 500, ExpireAt: soon}})

	got, ok := p.EarliestCreditExpiry("cn")
	if !ok || !got.Equal(soon) {
		t.Fatalf("pkgs-only signal: cn = %v ok=%v, want %v ok=true", got, ok, soon)
	}

	// 明细里的包全部过期（快照未刷新残留）→ 回落聚合快照（同样缺失）→ ok=false。
	p.SetCreditPackages("cn-pkgs", []PkgSnapshot{{Remain: 500, ExpireAt: time.Now().Add(-time.Hour)}})
	if _, ok := p.EarliestCreditExpiry("cn"); ok {
		t.Fatal("过期明细包不应产生信号")
	}

	// 明细与聚合快照并存时明细优先（NoteModelCost 实时扣减精度更高）。
	agg := time.Now().Add(10 * time.Hour).Truncate(time.Second)
	p.SetCreditPackages("cn-pkgs", []PkgSnapshot{{Remain: 100, ExpireAt: soon}})
	p.SetCreditsDetailed("cn-pkgs", 1000, 0, 0, agg, 500)
	got, ok = p.EarliestCreditExpiry("cn")
	if !ok || !got.Equal(soon) {
		t.Fatalf("pkgs should outrank aggregate snapshot: cn = %v ok=%v, want %v", got, ok, soon)
	}
}
