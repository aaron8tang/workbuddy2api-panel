// 包快照（PkgSnapshot）行为测试：SetCreditPackages 清洗/排序、consumePkgsLocked
// FEFO 扣减、pkgsExpiringFor 天粒度窗口口径、statusOf 透出派生值、state.json
// 落盘→恢复往返（含过期包惰性过滤）。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestSetCreditPackagesCleansAndSorts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()

	p.SetCreditPackages("u1", []PkgSnapshot{
		{Remain: 50, ExpireAt: now.Add(48 * time.Hour)},  // 保持
		{Remain: 0, ExpireAt: now.Add(time.Hour)},        // 零余额：剔除
		{Remain: -3, ExpireAt: now.Add(time.Hour)},       // 负余额：剔除
		{Remain: 20, ExpireAt: time.Time{}},              // 无到期时间：剔除
		{Remain: 10, ExpireAt: now.Add(-time.Minute)},    // 已过期：剔除
		{Remain: 30, ExpireAt: now.Add(2 * time.Hour)},   // 保持
		{Remain: 40, ExpireAt: now.Add(24 * time.Hour)},  // 保持
	})

	e := p.byUID["u1"]
	if len(e.pkgs) != 3 {
		t.Fatalf("清洗后应剩 3 个包，实际 %d", len(e.pkgs))
	}
	// 升序（FEFO）：2h → 24h → 48h。
	wantRemains := []int64{30, 40, 50}
	for i, w := range wantRemains {
		if e.pkgs[i].Remain != w {
			t.Fatalf("pkgs[%d].Remain = %d, want %d（应按 ExpireAt 升序）", i, e.pkgs[i].Remain, w)
		}
	}
	if !e.pkgs[0].ExpireAt.Before(e.pkgs[1].ExpireAt) || !e.pkgs[1].ExpireAt.Before(e.pkgs[2].ExpireAt) {
		t.Fatal("pkgs 应严格按 ExpireAt 升序")
	}

	// 不存在的 uid：空操作（不 panic）。
	p.SetCreditPackages("ghost", []PkgSnapshot{{Remain: 1, ExpireAt: now.Add(time.Hour)}})
}

func TestConsumePkgsLockedFEFO(t *testing.T) {
	now := time.Now()
	mk := func(remains []int64) *entry {
		e := &entry{}
		for i, r := range remains {
			e.pkgs = append(e.pkgs, pkgSnapshot{Remain: r, ExpireAt: now.Add(time.Duration(i+1) * time.Hour)})
		}
		return e
	}

	// 跨包扣减：包序 [100, 200, 300]，扣 150 → 包1 扣穿、包2 剩 150。
	e := mk([]int64{100, 200, 300})
	if used := consumePkgsLocked(e, 150); used != 150 {
		t.Fatalf("used = %d, want 150", used)
	}
	if len(e.pkgs) != 2 || e.pkgs[0].Remain != 150 || e.pkgs[1].Remain != 300 {
		t.Fatalf("扣减后 = %v, want [150 300]", e.pkgs)
	}

	// 恰好扣穿首包：包被删除，后续包不动。
	e = mk([]int64{100, 200})
	if used := consumePkgsLocked(e, 100); used != 100 {
		t.Fatalf("used = %d, want 100", used)
	}
	if len(e.pkgs) != 1 || e.pkgs[0].Remain != 200 {
		t.Fatalf("扣穿首包后 = %v, want [200]", e.pkgs)
	}

	// 扣穿全部：pkgs 置 nil，返回实际扣减总量。
	e = mk([]int64{100, 200})
	if used := consumePkgsLocked(e, 999); used != 300 {
		t.Fatalf("超扣 used = %d, want 300（受快照总余额钳制）", used)
	}
	if e.pkgs != nil {
		t.Fatalf("全部扣穿后 pkgs 应为 nil，实际 %v", e.pkgs)
	}

	// d<=0 / 空快照：无操作。
	e = mk([]int64{100})
	if used := consumePkgsLocked(e, 0); used != 0 || len(e.pkgs) != 1 {
		t.Fatalf("d=0 应无操作，used=%d len=%d", used, len(e.pkgs))
	}
	e = &entry{}
	if used := consumePkgsLocked(e, 10); used != 0 {
		t.Fatalf("空快照 used = %d, want 0", used)
	}
}

func TestPkgsExpiringForWindow(t *testing.T) {
	// 固定合成时刻（当天 10:00）：day(0)=当天正午必在未来、day(-1)=昨天正午必在
	// 过去，完全确定性——真实 time.Now 在 0-1 点或 23 点后运行会让 midnight 相对
	// 偏移的 day(0) 落到 now 之前、被当作陈旧包跳过。
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, time.Local)
	midnight := time.Date(2026, 3, 15, 0, 0, 0, 0, time.Local)
	// 天粒度窗口（winDays = winHours/24）：到期日 d 满足 d>=0 && d<=winDays 计入。
	// 24h 窗口 = d∈{0,1}（今天或明天到期）、48h 窗口 = d∈{0,1,2}——与前端
	// Math.floor 口径一致。day(-1)（陈旧快照，SetCreditPackages 已剔除，这里直测
	// 派生函数兜底）→ 跳过不计。
	day := func(days int) time.Time { return midnight.Add(time.Duration(days)*24*time.Hour + 12 * time.Hour) }
	pkgs := []pkgSnapshot{
		{Remain: 10, ExpireAt: day(0)},  // d=0：24h/48h 均计
		{Remain: 20, ExpireAt: day(1)},  // d=1：24h/48h 均计
		{Remain: 40, ExpireAt: day(2)},  // d=2：仅 48h 计
		{Remain: 80, ExpireAt: day(3)},  // d=3：均不计
		{Remain: 160, ExpireAt: day(-1)}, // 过去（陈旧快照）：不计
	}

	v24, ok := pkgsExpiringFor(pkgs, 24, now)
	if !ok || v24 != 30 {
		t.Fatalf("24h 窗口 = %d ok=%v, want 30 ok=true", v24, ok)
	}
	v48, ok := pkgsExpiringFor(pkgs, 48, now)
	if !ok || v48 != 70 {
		t.Fatalf("48h 窗口 = %d ok=%v, want 70 ok=true", v48, ok)
	}
	// 空快照：ok=false（statusOf 据此省略字段，前端回退旧数据源）。
	if _, ok := pkgsExpiringFor(nil, 24, now); ok {
		t.Fatal("空快照应返回 ok=false")
	}
	// 全部已过期的陈旧快照：窗口值 0 但 ok=true（有快照语义，前端显示真实 0）。
	if v, ok := pkgsExpiringFor([]pkgSnapshot{{Remain: 5, ExpireAt: day(-1)}}, 24, now); !ok || v != 0 {
		t.Fatalf("全过期快照 = %d ok=%v, want 0 ok=true", v, ok)
	}

	// 最早批次 = 首个未到期包的本地日期；陈旧过期包跳过。
	dayStr, ok := pkgsEarliestDay(pkgs, now)
	if !ok || dayStr != day(0).Format("2006-01-02") {
		t.Fatalf("earliest day = %q ok=%v, want %q", dayStr, ok, day(0).Format("2006-01-02"))
	}
	if _, ok := pkgsEarliestDay([]pkgSnapshot{{Remain: 5, ExpireAt: day(-1)}}, now); ok {
		t.Fatal("全过期快照应返回 ok=false（无未到期批次）")
	}
}

func TestStatusExposesPkgDerivedFields(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	// 到期时刻用 midnight 相对偏移（d=1 / d=2），消除「随当天时刻漂移」的 flaky：
	// d=1 恒在 24h 窗口（d∈{0,1}），d=2 恒在 48h 窗口但不在 24h 窗口。
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day1 := midnight.Add(24*time.Hour + time.Hour)
	day2 := midnight.Add(48*time.Hour + time.Hour)

	// 无快照：三个字段零值（omitempty 缺席）。
	if st, _ := p.Status("u1"); st.EarliestExpiryDay != "" {
		t.Fatalf("无快照时 earliest_expiry_day 应为空，实际 %q", st.EarliestExpiryDay)
	}

	p.SetCreditPackages("u1", []PkgSnapshot{
		{Remain: 149, ExpireAt: day1},
		{Remain: 60, ExpireAt: day2},
	})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("Status 应命中")
	}
	if st.Exp24h != 149 {
		t.Fatalf("exp_24h = %d, want 149（对应用户案例：消耗后应实时下降）", st.Exp24h)
	}
	if st.Exp48h != 209 {
		t.Fatalf("exp_48h = %d, want 209", st.Exp48h)
	}
	wantDay := day1.Format("2006-01-02")
	if st.EarliestExpiryDay != wantDay {
		t.Fatalf("earliest_expiry_day = %q, want %q", st.EarliestExpiryDay, wantDay)
	}

	// 消耗 50 点 → exp_24h 实时降到 99（NoteModelCost 内 consumePkgsLocked）。
	p.SetCredits("u1", 500, 0)
	p.NoteModelCost("u1", "test-model", 50, 1000)
	st, _ = p.Status("u1")
	if st.Exp24h != 99 {
		t.Fatalf("消耗后 exp_24h = %d, want 99", st.Exp24h)
	}
	// 跨包扣穿：再扣 99 → 首包耗尽，exp_24h 归 0 且最早批次跳到下一个包。
	p.NoteModelCost("u1", "test-model", 99, 1000)
	st, _ = p.Status("u1")
	if st.Exp24h != 0 || st.EarliestExpiryDay == wantDay {
		t.Fatalf("扣穿首包后 exp_24h = %d earliest = %q, want 0 且批次跳变", st.Exp24h, st.EarliestExpiryDay)
	}
}

func TestPackagesPersistRoundTrip(t *testing.T) {
	withNoPickGap(t)
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	live := now.Add(30 * time.Hour).Truncate(time.Second) // Truncate：JSON 往返时间戳精度对齐
	stale := now.Add(-time.Hour)
	p.SetCreditPackages("u1", []PkgSnapshot{
		{Remain: 149, ExpireAt: live},
		{Remain: 99, ExpireAt: stale}, // 已过期：落盘/恢复侧都应被过滤
	})
	p.Flush()

	// 落盘文件中出现 packages 字段（docker 重启防失忆的载体）。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			Packages []pkgSnapshot `json:"packages"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("unmarshal state.json: %v", err)
	}
	pkgs := sf.Accounts["u1"].Packages
	if len(pkgs) != 1 || pkgs[0].Remain != 149 {
		t.Fatalf("落盘 packages = %+v, want 仅剩未过期的 [149]", pkgs)
	}

	// 新池从同一文件恢复：过期包不复活，FEFO 序保持，statusOf 重新透出派生值。
	p2 := New(fp)
	e := p2.byUID["u1"]
	if len(e.pkgs) != 1 || e.pkgs[0].Remain != 149 {
		t.Fatalf("恢复后 pkgs = %v, want [149]", e.pkgs)
	}
	st, ok := p2.Status("u1")
	if !ok || st.Exp48h != 149 || st.EarliestExpiryDay == "" {
		t.Fatalf("恢复后 status exp_48h=%d earliest=%q ok=%v", st.Exp48h, st.EarliestExpiryDay, ok)
	}
	p2.Close()
	p.Close()
}
