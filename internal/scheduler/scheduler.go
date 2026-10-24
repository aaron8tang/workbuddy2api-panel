// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/stagger"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此做最早到期优先，见
	// entry.creditsEarliestExpiry）。<=0 时不做路由门槛；展示仍使用完整逐包数据。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool

	// IncludeDisabledInTasks 让「保号类」四任务（签到 / 活跃上报 / token 保活 / 余额刷新）
	// 对已禁用（disabled）的账号也执行（schedule.include_disabled_in_tasks）。
	// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到保号，但**仍不参与
	// 选号**——pool.pick 侧的 disabled 过滤与本开关无关。
	IncludeDisabledInTasks bool

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()

	// 渠道签到钩子（ChannelCheckinHooks）：主账号池之外的渠道（trae/qoderwork/
	// codearts/officeace/autoclaw）在签到时点一并执行各自的每日动作。
	//
	// 为什么是"钩子表 + func() []string"而不是直接持有各渠道网关类型：
	// scheduler 不 import 任何渠道包（渠道包不 import scheduler，但 panel 同时
	// import 两者），直接持有具体类型会把依赖方向钉死；钩子由 main 在各网关构造
	// 完成后挂载（网关 nil = 渠道关闭 = nil 钩子 = 到点跳过）。返回值是结果摘要
	// 行（每条一行，进运行日志），便于统一呈现。
	ChannelCheckinHooks []ChannelCheckinHook
}

// ChannelCheckinHook 一个渠道的签到钩子：Name 用于日志标识，Run 执行并返回
// 结果摘要行（每行一条，进运行日志）。
type ChannelCheckinHook struct {
	Name string
	Run  func() []string
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// batchMu 全局批量闸门：同一时刻只允许一个「遍历全部账号」的批量任务在跑
	// （签到/旅行/活跃/保活/余额/积分包，无论手动按钮、定时槽还是后台 ticker 触发）。
	// 防的是交叉并发：手动×定时、连点、同槽多族、余额 ticker 撞上签到等——
	// 账号间错峰只保证单批内部有序，两批交叉仍会同秒打同一账号。
	// 忙时 TryLock 失败直接拒绝：定时任务下轮再跑，余额/积分包晚一个周期无影响，
	// 手动点击收到「执行中」日志。零值可用（测试直接字面构造 Scheduler）。
	batchMu sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
	}
}

// ExpiringSoonWindow 返回当前快过期路由窗口（读取时与热配置写在 schedMu 下同步）。
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow 热更新快过期路由窗口。窗口变化时清空池内旧快照，避免在下一轮
// 余额刷新覆盖前，继续用旧窗口得出的最早到期顺序选号。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// SetChannelCheckinHooks 挂载渠道签到钩子表（main 在各渠道网关构造完成后调用；
// nil 或空表 = 只有主账号池参与签到）。钩子是装配期对象，改动需重启进程
//（与 trae/COSY/华为云/AutoClaw 渠道同口径，见 restartRequiredFields）。
func (s *Scheduler) SetChannelCheckinHooks(hooks []ChannelCheckinHook) {
	s.schedMu.Lock()
	s.cfg.ChannelCheckinHooks = hooks
	s.schedMu.Unlock()
}

// includeDisabledInTasks 返回保号类任务是否应覆盖禁用账号（配置快照，供循环开头取一次）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新「保号类任务是否覆盖禁用账号」
// （schedule.include_disabled_in_tasks）。
// 用独立 setter 而非并入 Reconfigure：后者已有 12 个位置参数，继续追加会让调用点难以
// 校对；本开关语义独立（只影响账号过滤，不影响时点），单独设值更清晰。
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 触发抖动（风控）：整点准点发起批量请求是机器人指纹（2026-10-23
			// 事故整改第二阶段），到点后随机延迟 0~5min 再跑。只挂在定时分支，
			// 手动入口不抖（用户点了就该立刻动）；迟到补跑同样抖（上游看不出
			// 这是补跑）。测试 factor=0 时 FireJitter 返回 0。
			if !sleepCtx(ctx, stagger.FireJitter()) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 串行执行本批全部任务族：各族依次跑完，跨族访问同账号有确定性
			// 间隔；族间不复用 waitGroup 并行（并行族之间没有账号间错峰保护，
			// 且闸门忙时会拒绝整族导致任务丢失）。闸门保证手动×定时×ticker
			// 全局互斥：拿不到说明已有别的批量在跑，本轮放弃，下轮再算。
			if s.tryAcquireBatch("定时批量") {
				s.runBatch(ctx, kinds)
				s.batchMu.Unlock()
			}
		}
	}
}

// runBatch 串行执行一批任务（同一唤醒时刻的多类任务）：调用方已持有 batchMu
// 闸门，这里直接直调各族实现（do*）。串行而非并行——并行族之间没有账号间错峰
// 保护，同一秒两族各打同一账号与单族内同秒等价；串行让跨族访问天然错开。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	for _, k := range kinds {
		switch k {
		case taskCheckin:
			s.doCheckin()
		case taskTravel:
			s.doTravel()
		case taskActivity:
			s.runActivity(ctx)
		case taskKeepalive:
			s.doKeepalive()
		case taskBlackcat:
			s.RunBlackcatNow()
		case taskGrowth:
			// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
			s.schedMu.Lock()
			hook := s.cfg.GrowthHook
			s.schedMu.Unlock()
			if hook != nil {
				hook()
			}
		}
	}
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// tryAcquireBatch 全局批量闸门：拿到即独占批量执行权（调用方负责 Unlock），
// 拿不到返回 false 并落一行拒绝日志（定时任务下轮再跑；手动入口由调用方提示忙）。
func (s *Scheduler) tryAcquireBatch(who string) bool {
	if s.batchMu.TryLock() {
		return true
	}
	log.Printf("scheduler: 批量任务执行中，%s 本轮跳过（闸门互斥）", who)
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻（面板手动入口）。
// 全局批量闸门：其他批量任务（定时族/余额 ticker/连点）执行中时直接拒绝，
// 防两批交叉同秒打同一账号（2026-10-23 事故整改第二阶段）。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用账号默认跳过——
// 若 schedule.include_disabled_in_tasks 打开则一并执行（禁用只关选号，不停保号）。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow() {
	if !s.tryAcquireBatch("手动签到") {
		return
	}
	defer s.batchMu.Unlock()
	s.doCheckin()
}

// doCheckin 签到遍历本体（调用方必须已持有 batchMu）：定时槽走 runBatch→本函数，
// 手动走 RunCheckinNow 包装；链式收尾连登管家与渠道签到同锁内执行。
func (s *Scheduler) doCheckin() {
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		// 账号间随机错峰：同渠道多账号绝不批量同刻打上游（2026-10-23 22:00
		// keepalive 同秒刷新 5 个账号的事故整改）。放在门控后、首个上游调用前，
		// 企业号（跳签到但仍查余额）同样错峰。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		// 企业版无签到体系：上游对 POST /v2/billing/meter/daily-checkin 直接
		// 400 code 10001「企业账号不支持该操作」。跳过签到，但**不 continue**——
		// 下方余额查询照常跑（企业额度走 get-enterprise-user-usage 口径，且是
		// credit_floor 判定与面板额度展示的唯一数据源）。
		if !a.IsEnterprise() {
			if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
				// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
				if upstream.IsAlreadyCheckin(err) {
					s.cfg.Pool.NoteCheckinDone(st.UID)
					log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
				} else {
					log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
				// 其余业务错误也继续走余额查询
			} else {
				// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
				// 重复触发时出现），成功也落一行。
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
			}
		}
		// 分桶查余额：配置窗口内的积分单独标记，同时记录最早未来到期批次。
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}
	s.RunStreakBonusNow()
	// 渠道签到：主池之外的渠道在同一时点执行各自每日动作（钩子由 main 装配时
	// 挂载，网关 nil = 渠道关闭 = 无钩子）。每个渠道独立 recover——一个渠道
	// panic 不能带走整轮签到（渠道包是后集成的，健壮性参差）。
	// 注意：这里仍在 batchMu 闸门内——渠道 CheckinAll 自带各自的渠道级互斥，
	// 与本闸门分层（主池批量 vs 渠道批量），不冲突。
	s.runChannelCheckins()
}

// runChannelCheckins 逐个跑渠道签到钩子，把结果摘要写进运行日志。
func (s *Scheduler) runChannelCheckins() {
	s.schedMu.Lock()
	hooks := s.cfg.ChannelCheckinHooks
	s.schedMu.Unlock()
	for _, h := range hooks {
		if h.Run == nil {
			continue
		}
		name := h.Name
		if name == "" {
			name = "channel"
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("checkin %s: panic: %v", name, r)
				}
			}()
			lines := h.Run()
			if len(lines) == 0 {
				log.Printf("checkin %s: 无账号或无结果", name)
				return
			}
			for _, line := range lines {
				log.Printf("checkin %s: %s", name, line)
			}
		}()
	}
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报（面板手动入口，
// 全局批量闸门互斥，忙时拒绝）。
// 禁用账号默认跳过（schedule.include_disabled_in_tasks 打开时一并上报）；
// 无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上
// code=0 OK，点亮连登）；一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	if !s.tryAcquireBatch("手动活跃上报") {
		return
	}
	defer s.batchMu.Unlock()
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// 企业版跳过活跃上报：/v2/report 本身返回 200（不报错），但它唯一的作用是
		// 点亮 growth 连登天数 / 解锁 first_buddy 领养——这两者对企业号都是 403
		// （「growth system is only available for personal users」），即**零收益**。
		// 该类"静默无效"最易被忽略（日志无异常），故显式跳过，不给上游多发一次请求。
		if a.IsEnterprise() {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切，
		// 无需改动 upstream。此处曾按「D4 门控：global 无活跃体系」跳过 global，实测该
		// 判断不成立——国际版 /v2/report 可用，跳过即国际版账号永远点不亮连登（上游
		// a190252 同口径修复）。checkin/travel 的 global 门控不受影响，仍跳过。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用（面板手动入口，
// 全局批量闸门互斥，忙时拒绝）。禁用账号默认跳过；若 schedule.include_disabled_in_tasks
// 打开，禁用号也会续期 token
// （这是轮换用法下把闲置号保持可用的关键），但**不再对已禁用的号重复计数 12153**——
// 它已经是终态，再计一次只会打出一行「— 禁用」的误导日志。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	if !s.tryAcquireBatch("手动保活") {
		return
	}
	defer s.batchMu.Unlock()
	s.doKeepalive()
}

// doKeepalive 保活遍历本体（调用方必须已持有 batchMu）。
func (s *Scheduler) doKeepalive() {
	includeDisabled := s.includeDisabledInTasks()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 账号间随机错峰：2026-10-23 22:00:01 同秒例行刷新 5 个账号——串行但
		// 零延迟在快上游下就是同一秒，是典型机器人特征。改为 3~8s 随机间隔。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead && !st.Disabled {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunBalanceRefreshNow 串行查询余额并更新池内 credits（账号间随机错峰）。
// 全局批量闸门互斥：余额 ticker / 面板手动刷新 / 其他批量任务三方互斥，
// 忙时拒绝并返回 false（面板据此回 409「执行中」；ticker 侧晚一周期无影响）。
// 并发扇出是 2026-10-23 会话批量吊销事故的头号风险点——同渠道多账号同一窗口
// 打同一接口是典型机器人特征，改为串行 + Light 档错峰。账号范围：默认跳过
// 禁用账号；schedule.include_disabled_in_tasks 打开时一并刷新
// （轮换用法下据此判断下一个该启用谁）。注意 ReenableIfCredits 对 disabled 是
// no-op，所以本开关**不会**导致禁用号被自动解冻——它只让已有账号的积分保持新鲜。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() bool {
	if !s.tryAcquireBatch("余额刷新") {
		return false
	}
	defer s.batchMu.Unlock()
	s.doBalanceRefresh()
	return true
}

// doBalanceRefresh 余额刷新遍历本体（调用方必须已持有 batchMu）。
func (s *Scheduler) doBalanceRefresh() {
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if !first {
			time.Sleep(stagger.Light())
		}
		first = false
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
		if err != nil {
			log.Printf("balance %s: %v", logfmt.Label(st.UID, a.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

// RunPackageRefreshNow 串行拉取全部账号的积分包明细并写入池内本地快照
// （pool.SetCreditPackages），账号间随机错峰（同 doBalanceRefresh：
// goroutine 并发扇出已废，同渠道多账号绝不并发打上游）。全局批量闸门互斥，
// 忙时拒绝（每日任务晚点再跑无影响）。账号范围口径同
// RunBalanceRefreshNow：默认跳过禁用账号；include_disabled_in_tasks 打开时
// 一并刷新。失败账号保留旧快照（SetCreditPackages 仅成功路径调用），下次
// 每日刷新再补。供每日定时任务（StartPackageRefresh）与启动预热调用；
// 面板「积分构成」的实时查询走 panel.packages()（现打上游），两者不共用。
func (s *Scheduler) RunPackageRefreshNow() {
	if !s.tryAcquireBatch("积分包刷新") {
		return
	}
	defer s.batchMu.Unlock()
	s.doPackageRefresh()
}

// doPackageRefresh 积分包刷新遍历本体（调用方必须已持有 batchMu）。
func (s *Scheduler) doPackageRefresh() {
	includeDisabled := s.includeDisabledInTasks()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if !first {
			time.Sleep(stagger.Light())
		}
		first = false
		packs, _, _, err := s.cfg.Upstream.CreditPackages(a)
		if err != nil {
			log.Printf("pkgrefresh %s: %v", logfmt.Label(st.UID, a.Nickname), err)
			continue
		}
		pkgs := make([]pool.PkgSnapshot, 0, len(packs))
		for _, cp := range packs {
			// 失效时刻：ExpiresAt（Unix 毫秒，DeductionEndTime 优先口径）权威；
			// 0 时回退解析 EndTime（上游墙钟 "2006-01-02 15:04:05"，UTC+8；
			// RFC3339 形态经 time.Parse 自然兜底）。两者皆缺的包不计入快照。
			var exp time.Time
			if cp.ExpiresAt > 0 {
				exp = time.UnixMilli(cp.ExpiresAt)
			} else if cp.EndTime != "" {
				if t, perr := time.ParseInLocation("2006-01-02 15:04:05", cp.EndTime, time.FixedZone("UTC+8", 8*60*60)); perr == nil {
					exp = t
				} else if t, perr := time.Parse(time.RFC3339, cp.EndTime); perr == nil {
					exp = t
				}
			}
			if exp.IsZero() {
				continue
			}
			pkgs = append(pkgs, pool.PkgSnapshot{Remain: cp.Remain, ExpireAt: exp})
		}
		s.cfg.Pool.SetCreditPackages(st.UID, pkgs)
	}
}

// StartPackageRefresh 每日积分包明细刷新（独立 ticker goroutine，ctx 取消即停）。
// 包明细是慢变量（面额/失效时刻只随发放/到期变化），每天拉一次足够——消耗引发的
// 余额变化由 NoteModelCost 在本地快照上实时 FEFO 扣减，不打上游。每天 05:20 触发
// （避开整点任务群：签到 9/21、活跃 10、保活 22、余额刷新每 5 分钟；跨日边界
// 过期包由快照惰性过滤剔除，05:20 拉取后新一天口径即正确）。
// interval<=0 不启动（每日一次无热改需求，不做 interval 配置面）。
func (s *Scheduler) StartPackageRefresh(ctx context.Context) {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 5, 20, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			timer := time.NewTimer(next.Sub(now))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.RunPackageRefreshNow()
			}
		}
	}()
}
