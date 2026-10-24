package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

func TestPickHighestCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 300)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "plan limit" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolPlan, time.Hour, "plan limit")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestSyncToDirRemovesMissing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestRemove(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.Remove("u1") {
		t.Fatal("remove existing should return true")
	}
	if p.Pick() != nil {
		t.Fatal("u1 should be gone after remove")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 status should not exist")
	}
	if p.Remove("u1") {
		t.Fatal("remove missing should return false")
	}
}

// TestRemoveClearsStateEntry Remove 后 state.json 不再含该条目；
// 模拟「auths 文件也已删」场景：reload（不再 Add 该 uid）后池中无该账号。
func TestRemoveClearsStateEntry(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100) // 触发 saveLocked 落盘
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 50)
	p.Remove("u1")
	// reload：不再 Add u1（等价于 auths/trae-u1.json 已被 API 层删除）
	p2 := New(fp)
	if _, ok := p2.Status("u1"); ok {
		t.Fatal("u1 should not be in state after remove + reload")
	}
	if _, ok := p2.Status("u2"); !ok {
		t.Fatal("u2 should remain in state")
	}
	// u2 从 state reload，credits 应保留
	st, _ := p2.Status("u2")
	if st.Credits != 50 {
		t.Errorf("u2 credits lost on reload: %+v", st)
	}
}

func TestSetEnabledSoftSwitch(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	if !p.SetEnabled("u1", false, "user paused") {
		t.Fatal("set enabled on existing should return true")
	}
	if p.Pick() != nil {
		t.Fatal("disabled-by-user account should be skipped by Pick")
	}
	st, _ := p.Status("u1")
	if st.Enabled {
		t.Errorf("Enabled flag should be false, got %+v", st)
	}
	if st.Disabled {
		t.Errorf("soft switch must not set Disabled (session dead), got %+v", st)
	}
	if st.Reason != "user paused" {
		t.Errorf("reason should be recorded, got %+v", st)
	}
	// 重新启用
	p.SetEnabled("u1", true, "")
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("re-enabled account should be picked, got %+v", got)
	}
	st2, _ := p.Status("u1")
	if !st2.Enabled {
		t.Errorf("Enabled should be true after re-enable, got %+v", st2)
	}
}

func TestSetEnabledDoesNotAffectDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	// session-dead 硬禁用时切软开关不应当作恢复
	p.SetEnabled("u1", true, "")
	if p.Pick() != nil {
		t.Fatal("disabled (session dead) must stay un-pickable even if enabled=true")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("Disabled should remain true, got %+v", st)
	}
}

func TestSetEnabledOnMissing(t *testing.T) {
	p := New("")
	if p.SetEnabled("nope", false, "x") {
		t.Fatal("set enabled on missing should return false")
	}
}

func TestSetEnabledPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetEnabled("u1", false, "paused")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("soft-disabled account should stay skipped after reload")
	}
	st, _ := p2.Status("u1")
	if st.Enabled || st.Reason != "paused" {
		t.Errorf("soft switch state lost: %+v", st)
	}
}

// TestStateFileBackwardCompat 旧 state.json 无 enabled 字段时，reload 默认按启用处理。
func TestStateFileBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 手写一份无 enabled 字段的旧格式
	old := `{"accounts":{"u1":{"credits":100,"disabled":false,"reason":""}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 should load from old state")
	}
	if !st.Enabled {
		t.Errorf("missing enabled field should default to true, got %+v", st)
	}
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("old-format account should be pickable, got %+v", got)
	}
}

func TestPickWorkHighestCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetWorkCredits("u1", 50.5)
	p.SetWorkCredits("u2", 200.1)
	p.SetWorkCredits("u3", 120.0)

	got := p.PickWork()
	if got == nil || got.UID != "u2" {
		t.Fatalf("PickWork should pick u2 (200.1 credits), got %v", got)
	}

	// 排除 u2，应该轮换到 u3
	got2 := p.PickWorkExcluding(map[string]bool{"u2": true})
	if got2 == nil || got2.UID != "u3" {
		t.Fatalf("PickWorkExcluding should pick u3 (120 credits), got %v", got2)
	}

	// 再次排除 u3，应该轮换到 u1
	got3 := p.PickWorkExcluding(map[string]bool{"u2": true, "u3": true})
	if got3 == nil || got3.UID != "u1" {
		t.Fatalf("PickWorkExcluding should pick u1 (50.5 credits), got %v", got3)
	}

	// 全部排除返回 nil
	got4 := p.PickWorkExcluding(map[string]bool{"u1": true, "u2": true, "u3": true})
	if got4 != nil {
		t.Fatalf("all excluded should return nil, got %v", got4)
	}
}

func TestWorkCooldownAndIndependentFromSOLO(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500)
	p.SetWorkCredits("u1", 100.0)

	// SOLO 进入 1005 长冷却
	p.Cooldown("u1", CoolPlan, 10*time.Minute, "solo plan limit")

	// SOLO 应该不可被挑选
	if p.Pick() != nil {
		t.Fatal("u1 should not be picked for SOLO during CoolPlan")
	}

	// 但 Work 通道仍然可用！
	if p.PickWork() == nil || p.PickWork().UID != "u1" {
		t.Fatal("u1 should still be pickable for Work while SOLO is in CoolPlan")
	}

	// 现在对 Work 进行冷却
	p.CooldownWork("u1", 10*time.Minute, "work credits empty")
	if p.PickWork() != nil {
		t.Fatal("u1 should not be picked for Work during CooldownWork")
	}

	st, _ := p.Status("u1")
	if !st.WorkCooling || st.WorkReason != "work credits empty" {
		t.Errorf("expected WorkCooling true, got %+v", st)
	}
}

// TestPickFollowsCredits 无粘性语义：每次选号即时按当前积分择优，积分变化
// 下一跳立即生效（消耗后重排的前提），冷却账号自动让位。
func TestPickFollowsCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 200)

	// 首选 u2
	got1 := p.PickExcluding(nil)
	if got1 == nil || got1.UID != "u2" {
		t.Fatalf("expected u2, got %+v", got1)
	}

	// 无粘性：u1 积分上升后下一跳立即切到 u1（消耗后重排语义）
	p.SetCredits("u1", 999)
	got2 := p.PickExcluding(nil)
	if got2 == nil || got2.UID != "u1" {
		t.Fatalf("expected immediate switch to u1 (no stickiness), got %+v", got2)
	}

	// 排除 u1（请求级轮转）→ u2
	if g := p.PickExcluding(map[string]bool{"u1": true}); g == nil || g.UID != "u2" {
		t.Fatalf("expected u2 after u1 excluded, got %+v", g)
	}

	// u2 冷却时只剩 u1
	p.Cooldown("u2", CoolSoft, time.Minute, "rate limit")
	if g := p.PickExcluding(nil); g == nil || g.UID != "u1" {
		t.Fatalf("expected u1 after u2 cooldown, got %+v", g)
	}
}

func TestCheckinGenerationRotateOnly(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if g := p.CheckinGeneration("u1"); g != 0 {
		t.Fatalf("initial generation=%d want 0", g)
	}
	if g := p.BumpCheckinGeneration("u1"); g != 1 {
		t.Fatalf("after bump generation=%d want 1", g)
	}
	// 上游从不清零代数（只清退避）：轮换后保持，下次继续用新设备 ID。
	if g := p.CheckinGeneration("u1"); g != 1 {
		t.Fatalf("generation=%d want sticky 1 (never reset)", g)
	}
	if g := p.CheckinGeneration("missing"); g != 0 {
		t.Fatalf("missing account generation=%d want 0", g)
	}
	if g := p.BumpCheckinGeneration("missing"); g != 0 {
		t.Fatalf("bump missing should be 0, got %d", g)
	}
}

func TestCheckinRetryBackoffGrowsAndCaps(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.CheckinRetryDue("u1") {
		t.Fatal("fresh account must not be retry-due (daily run owns first attempt)")
	}
	after1 := p.NoteCheckinRateLimited("u1")
	if p.CheckinRetryDue("u1") {
		t.Fatal("just-recorded backoff must not be due")
	}
	d1 := time.Until(after1)
	if d1 < 50*time.Second || d1 > 70*time.Second {
		t.Errorf("first backoff=%v want ~60s", d1)
	}
	// 退避按 60→120→240→480 增长并封顶（Source: Trae2api-cn @ 165ac6e）。
	want := []time.Duration{120 * time.Second, 240 * time.Second, 480 * time.Second, 480 * time.Second}
	for i, w := range want {
		after := p.NoteCheckinRateLimited("u1")
		if d := time.Until(after); d < w-10*time.Second || d > w+10*time.Second {
			t.Errorf("backoff step %d=%v want ~%v", i+2, d, w)
		}
	}
	p.ClearCheckinRetry("u1")
	if p.CheckinRetryDue("u1") {
		t.Fatal("cleared retry must not be due")
	}
}

func TestCheckinRetryPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinRateLimited("u1")
	q := New(fp)
	q.Add(&auth.Auth{UID: "u1"})
	if q.CheckinRetryDue("u1") {
		t.Fatal("reloaded backoff must still be pending (restart must not re-hammer upstream)")
	}
}

func TestCheckinGenerationPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.BumpCheckinGeneration("u1")
	p.BumpCheckinGeneration("u1")
	q := New(fp)
	q.Add(&auth.Auth{UID: "u1"})
	if g := q.CheckinGeneration("u1"); g != 2 {
		t.Fatalf("reloaded generation=%d want 2", g)
	}
}

// TestAccountOrderDedupAndClear SetAccountOrder 的去重、空项与清空恢复自动模式。
func TestAccountOrderDedupAndClear(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.Add(&auth.Auth{UID: "c"})
	p.SetCredits("a", 100)
	p.SetCredits("b", 200)
	p.SetCredits("c", 300)

	// 全未排序：自动模式选积分最高
	if got := p.Pick(); got == nil || got.UID != "c" {
		t.Fatalf("auto mode should pick highest credits c, got %+v", got)
	}

	// 去重 + 空项防御：b 重复出现只记首个名次
	p.SetAccountOrder([]string{"", "b", "b", "a"})
	if !p.AccountOrderActive() {
		t.Fatal("order should be active")
	}
	got := p.AccountOrder()
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("account order=%v want [b a]", got)
	}
	// 未覆盖的 c 垫底；rank 压过积分 → b 排第一
	if g := p.Pick(); g == nil || g.UID != "b" {
		t.Fatalf("rank should beat credits, got %+v", g)
	}
	// rank a=2 < 垫底 c → 排除 b 后选 a 而非积分更高的 c
	if g := p.PickExcluding(map[string]bool{"b": true}); g == nil || g.UID != "a" {
		t.Fatalf("rank a=2 should beat unranked c, got %+v", g)
	}

	// 清空恢复自动模式
	p.SetAccountOrder(nil)
	if p.AccountOrderActive() {
		t.Fatal("order should be inactive after clear")
	}
	if g := p.Pick(); g == nil || g.UID != "c" {
		t.Fatalf("after clear should fall back to highest credits c, got %+v", g)
	}
}

// TestAccountOrderPersistsAcrossReload 手工顺序挂 state 条目持久化。
func TestAccountOrderPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetAccountOrder([]string{"a", "b"})
	q := New(fp)
	q.Add(&auth.Auth{UID: "a"})
	q.Add(&auth.Auth{UID: "b"})
	if !q.AccountOrderActive() {
		t.Fatal("order should survive reload")
	}
	got := q.AccountOrder()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("reloaded order=%v want [a b]", got)
	}
}

// TestAccountOrderGovernsPick 顺序模式经 PickExcluding 生效：
// 命中顺序第一名（rank 压过三因子权重），顺序第一名冷却后轮到第二名。
func TestAccountOrderGovernsPick(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.Add(&auth.Auth{UID: "c"})
	p.SetCredits("a", 100)
	p.SetCredits("b", 200)
	p.SetCredits("c", 300)

	p.SetAccountOrder([]string{"b", "c", "a"})
	if g := p.PickExcluding(nil); g == nil || g.UID != "b" {
		t.Fatalf("should hit order first b, got %+v", g)
	}
	// 顺序第一名冷却 → 应命中第二名 c（rank 次序保持）
	p.Cooldown("b", CoolPlan, time.Hour, "x")
	if g := p.PickExcluding(nil); g == nil || g.UID != "c" {
		t.Fatalf("order second c should be picked after b cools, got %+v", g)
	}
}

// TestWorkOrderRankPriority Work 通道同样受手工顺序约束。
func TestWorkOrderRankPriority(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetWorkCredits("a", 500)
	p.SetWorkCredits("b", 10)
	p.SetAccountOrder([]string{"b"})
	if g := p.PickWork(); g == nil || g.UID != "b" {
		t.Fatalf("work pick should follow manual order b, got %+v", g)
	}
}

// expPickAt 以「今天 12:00」为基准返回 now+days 天后的 Unix 秒（12:00 避开
// 日界边界；days≥1 恒在未来。days=0 午后运行会翻车为过去时刻，禁止使用）。
func expPickAt(days int) int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location()).AddDate(0, 0, days).Unix()
}

// withNoPickGap 临时关闭防并发撞号窗口（minPickGap=0），让连续 Pick 的确定性
// 断言不受 100ms 防撞窗口影响（与主池 pool_test.go 同约定）。
func withNoPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })
}

// TestPickAffinityExpiringHardTier 自动模式下存在未来批次积分的账号硬优先收敛
// （issue 实案：积分最高 4600、最早包 19 天后才到期的远期号 25567921 霸占选号，
// 117/24 积分的临期号被晾着）。收敛集内按 FEFO 先用最早失效的（精确到时分秒），
// 无未来批次的账号（none）中签率为 0。
func TestPickAffinityExpiringHardTier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "far"})  // 4600 积分，19 天后到期
	p.Add(&auth.Auth{UID: "exp1"}) // 117 积分，明天到期
	p.Add(&auth.Auth{UID: "exp0"}) // 24 积分，2 分钟后到期（FEFO 首选）
	p.Add(&auth.Auth{UID: "none"}) // 无权益包（fefe=0，被硬过滤剔除）
	p.SetCredits("far", 4600)
	p.SetCredits("exp1", 117)
	p.SetCredits("exp0", 24)
	p.SetCredits("none", 50)
	p.SetCreditExpiry("far", expPickAt(19), []CreditPack{{ExpireAt: expPickAt(19), Remain: 4600}})
	p.SetCreditExpiry("exp1", expPickAt(1), []CreditPack{{ExpireAt: expPickAt(1), Remain: 117}})
	soon := time.Now().Add(2 * time.Minute).Unix()
	p.SetCreditExpiry("exp0", soon, []CreditPack{{ExpireAt: soon, Remain: 24}})

	// 硬过滤收敛到 {far, exp1, exp0}，FEFO 先选最早失效的 exp0
	if g := p.PickExcluding(nil); g == nil || g.UID != "exp0" {
		t.Fatalf("expiring hard tier should pick exp0 (earliest expiry), got %+v", g)
	}
	// 排除 exp0（请求级轮转）→ exp1；积分最高的 far 必须排在其后
	if g := p.PickExcluding(map[string]bool{"exp0": true}); g == nil || g.UID != "exp1" {
		t.Fatalf("exp1 should be picked after exp0 excluded; far must lose, got %+v", g)
	}
	// 再排除 exp1 → 轮到 far（远期批次仍排在无批次 none 之前）
	if g := p.PickExcluding(map[string]bool{"exp0": true, "exp1": true}); g == nil || g.UID != "far" {
		t.Fatalf("far should be picked after exp0/exp1 excluded; none must lose, got %+v", g)
	}
}

// TestPickAffinitySecondsFEFO 秒级 FEFO：不同未来时刻（时分秒不同）的账号按最早
// 失效时刻升序轮询；已过期与扣穿的批次包不参与排序（earliestPackExpiry 跳过）。
func TestPickAffinitySecondsFEFO(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "early"}) // 90 秒后失效（多包中含过期/扣穿死包，应被跳过）
	p.Add(&auth.Auth{UID: "late"})  // 3 分钟后失效
	p.Add(&auth.Auth{UID: "none"})  // 无权益包
	now := time.Now()
	p.SetCredits("early", 100)
	p.SetCredits("late", 200)
	p.SetCredits("none", 50)
	p.SetCreditExpiry("early", now.Add(90*time.Second).Unix(), []CreditPack{
		{ExpireAt: now.Add(-1 * time.Hour).Unix(), Remain: 30}, // 已过期 → 跳过
		{ExpireAt: now.Add(90 * time.Second).Unix(), Remain: 50},
		{ExpireAt: now.Add(90 * time.Second).Unix(), Remain: 0}, // 扣穿 → 跳过
	})
	p.SetCreditExpiry("late", now.Add(3*time.Minute).Unix(), []CreditPack{
		{ExpireAt: now.Add(3 * time.Minute).Unix(), Remain: 200},
	})

	for i := 0; i < 10; i++ {
		if g := p.Pick(); g == nil || g.UID != "early" {
			t.Fatalf("iter %d: pick=%+v, want early (expires 90s before late)", i, g)
		}
	}
	// none（无未来批次）不能插队
	if g := p.PickExcluding(map[string]bool{"early": true, "late": true}); g == nil || g.UID != "none" {
		t.Fatalf("none should only be reachable when both expiring accounts excluded, got %+v", g)
	}
}

// TestPickAffinityExpiringYieldsToManualOrder 手工使用顺序压过临期硬偏好：
// 用户手工拖到第一位的远期号必须可被选中（与主池粘性分配路径同约定，
// 硬过滤只属于自动策略）。
func TestPickAffinityExpiringYieldsToManualOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "far"})
	p.Add(&auth.Auth{UID: "exp1"})
	p.SetCredits("far", 4600)
	p.SetCredits("exp1", 117)
	p.SetCreditExpiry("far", expPickAt(19), []CreditPack{{ExpireAt: expPickAt(19), Remain: 4600}})
	p.SetCreditExpiry("exp1", expPickAt(1), []CreditPack{{ExpireAt: expPickAt(1), Remain: 117}})

	p.SetAccountOrder([]string{"far"})
	if g := p.PickExcluding(nil); g == nil || g.UID != "far" {
		t.Fatalf("manual order must beat expiring hard tier, got %+v", g)
	}
}

// TestPickAffinityNoExpiringNoRegression 全员无窗口内到期积分（明细存在但都是
// 远期包）时退化为既有三因子行为：积分最高者胜出，零回归。
func TestPickAffinityNoExpiringNoRegression(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 100)
	p.SetCredits("b", 4600)
	p.SetCreditExpiry("a", expPickAt(19), []CreditPack{{ExpireAt: expPickAt(19), Remain: 100}})
	p.SetCreditExpiry("b", expPickAt(19), []CreditPack{{ExpireAt: expPickAt(19), Remain: 4600}})
	if g := p.PickExcluding(nil); g == nil || g.UID != "b" {
		t.Fatalf("no expiring candidates should fall back to weight pick b, got %+v", g)
	}
}

// TestDeductCreditsFEFO 内存预扣按最早到期优先扣减权益包：早到期包清空后溢出到
// 晚到期包；聚合余额同步下降；未知倍率/零 tokens 直接跳过不产生任何变化。
func TestDeductCreditsFEFO(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000)
	p.SetCreditExpiry("u1", expPickAt(10), []CreditPack{
		{ExpireAt: expPickAt(2), Remain: 100},  // 早到期包
		{ExpireAt: expPickAt(10), Remain: 900}, // 晚到期包
	})

	// rate=1.0 × 1e6 tokens × calib 100 = 100 积分 → 早到期包整包扣穿
	p.DeductCredits("u1", 1.0, 1_000_000)
	st, _ := p.Status("u1")
	if st.Credits != 900 {
		t.Fatalf("credits=%d want 900 (calib 100 × rate 1.0 × 1M tok)", st.Credits)
	}
	if len(st.CreditPacks) != 2 {
		t.Fatalf("packs=%d want 2", len(st.CreditPacks))
	}
	if st.CreditPacks[0].Remain != 0 {
		t.Errorf("early pack remain=%v want 0 (FEFO first)", st.CreditPacks[0].Remain)
	}
	if st.CreditPacks[1].Remain != 900 {
		t.Errorf("late pack remain=%v want 900 (untouched)", st.CreditPacks[1].Remain)
	}

	// 再扣 50：早包已空，溢出到晚到期包
	p.DeductCredits("u1", 1.0, 500_000)
	st, _ = p.Status("u1")
	if st.Credits != 850 {
		t.Fatalf("credits=%d want 850", st.Credits)
	}
	if st.CreditPacks[1].Remain != 850 {
		t.Errorf("late pack remain=%v want 850 (overflow deduction)", st.CreditPacks[1].Remain)
	}

	// 未知倍率与零 tokens 均跳过
	p.DeductCredits("u1", 0, 500_000)
	p.DeductCredits("u1", 1.0, 0)
	st, _ = p.Status("u1")
	if st.Credits != 850 {
		t.Fatalf("unknown rate / zero tokens must be no-ops, credits=%d", st.Credits)
	}
}

// TestCreditCalibration 权威余量差 EMA 校准闭环：
// 预扣估算偏差 → 下次权威刷新时按 delta/sample 推观测值 → EMA 0.5 混合进系数。
// 样本不足（<1e5 rateTokens）与赠送回补（delta<=0）不构成样本。
func TestCreditCalibration(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000)
	future := expPickAt(19)

	// 建立基线快照（首次 SetCreditExpiry 无旧快照，不触发校准）
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 1000}})
	if got := p.CreditCalib(); got != 100.0 {
		t.Fatalf("initial calib=%v want 100", got)
	}

	// 预扣：rate 0.5 × 200k tokens × calib 100 = 10 积分；样本 rateTokens = 1e5
	p.DeductCredits("u1", 0.5, 200_000)

	// 权威刷新显示真实消耗 15 积分（估算低估）：
	// obs = (1000-10-985) / (1e5/1e6) = 5/0.1 = 50；EMA: 0.5×100 + 0.5×50 = 75
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 985}})
	if got := p.CreditCalib(); got != 75.0 {
		t.Fatalf("calibrated calib=%v want 75", got)
	}

	// 样本不足跳过：rate 0.5 × 100k = 5e4 < 1e5 rateTokens，即便余量差不构成样本
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 980}})
	if got := p.CreditCalib(); got != 75.0 {
		t.Fatalf("undersized sample must not calibrate, calib=%v want 75", got)
	}

	// 赠送/回补（权威余量高于内存扣减值 → delta<=0）不构成样本
	p.DeductCredits("u1", 0.5, 400_000) // 样本 2e5，按 calib 75 扣 15 → 内存 965
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 980}})
	if got := p.CreditCalib(); got != 75.0 {
		t.Fatalf("delta<=0 must not calibrate, calib=%v want 75", got)
	}
}

// TestCreditCalibrationSkipsExpiredPack 两次同步间有权益包到期清退时跳过校准：
// 余量差混入「到期作废」非纯消费，观测值会系统性偏大。
func TestCreditCalibrationSkipsExpiredPack(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 600)
	soon := time.Now().Add(1 * time.Second).Unix()
	future := expPickAt(19)

	// 基线含 1s 后到期的临期包
	p.SetCreditExpiry("u1", soon, []CreditPack{
		{ExpireAt: soon, Remain: 100},
		{ExpireAt: future, Remain: 500},
	})
	p.DeductCredits("u1", 0.5, 200_000) // 预扣 10 积分，样本 1e5

	time.Sleep(1200 * time.Millisecond) // 临期包到期

	// 权威刷新：余量差同时含消费与到期作废 → 必须跳过校准
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 485}})
	if got := p.CreditCalib(); got != 100.0 {
		t.Fatalf("expired pack window must skip calibration, calib=%v want 100", got)
	}
}

// TestCreditCalibPersistsAcrossReload 池级校准系数持久化：重启不丢校准结果。
func TestCreditCalibPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	future := expPickAt(19)
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 1000}})
	p.DeductCredits("u1", 0.5, 200_000)
	p.SetCreditExpiry("u1", future, []CreditPack{{ExpireAt: future, Remain: 985}})
	if got := p.CreditCalib(); got != 75.0 {
		t.Fatalf("calibrated calib=%v want 75", got)
	}

	q := New(fp)
	if got := q.CreditCalib(); got != 75.0 {
		t.Fatalf("reloaded calib=%v want 75 (persisted)", got)
	}
}
