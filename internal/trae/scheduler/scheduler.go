// Package scheduler trae 子系统定时任务：每日签到 + token 预刷新 + 9074 退避重试。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
// 与 workbuddy 主调度器（internal/scheduler）完全独立：独立池、独立上游、独立排程。
package scheduler

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/stagger"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool              *pool.Pool
	Upstream          *upstream.Client
	CheckinHour       int           // 每日签到小时，默认 9
	RefreshHours      []int         // token 预刷新小时，默认 [3]
	RefreshSkew       time.Duration // 预刷新窗口，默认 24h
	CreditRefreshEvery time.Duration // 积分明细定点刷新间隔，默认 6h
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// batchMu 全局批量闸门（与主池 scheduler 同口径，2026-10-23 事故整改第二阶段）：
	// 签到/重试/积分刷新/token 预刷新四类批量互斥——RetryLoop 每 15min tick 与
	// 9 点签到、CreditsLoop 启动即刷与定时签到等都可能交叉，两批并行会同窗口
	// 打同一账号。忙时 TryLock 失败直接拒绝（定时任务下轮再跑）。零值可用。
	batchMu sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.CreditRefreshEvery <= 0 {
		cfg.CreditRefreshEvery = 6 * time.Hour
	}
	return &Scheduler{cfg: cfg}
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

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 触发抖动（风控，2026-10-23 事故整改第二阶段）：整点准点批量请求是
			// 机器人指纹，到点后随机延迟 0~5min 再跑。测试 factor=0 时返回 0。
			if !sleepCtx(ctx, stagger.FireJitter()) {
				return
			}
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				if s.tryAcquireBatch("trae 定时刷新") {
					s.doRefresh()
					s.batchMu.Unlock()
				}
			}
			if s.cfg.CheckinHour == h {
				if s.tryAcquireBatch("trae 定时签到") {
					s.doCheckin()
					s.batchMu.Unlock()
				}
			}
		}
	}
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false，等满返回 true。
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

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// tryAcquireBatch 全局批量闸门：拿到即独占批量执行权（调用方负责 Unlock），
// 拿不到返回 false 并落一行拒绝日志（定时任务下轮再跑）。
func (s *Scheduler) tryAcquireBatch(who string) bool {
	if s.batchMu.TryLock() {
		return true
	}
	log.Printf("[trae] scheduler: 批量任务执行中，%s 本轮跳过（闸门互斥）", who)
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻（手动入口，闸门互斥忙时拒绝）。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	if !s.tryAcquireBatch("trae 手动签到") {
		return
	}
	defer s.batchMu.Unlock()
	s.doCheckin()
}

// doCheckin 签到遍历本体（调用方必须已持有 batchMu）：定时槽走 Run() → 本函数。
func (s *Scheduler) doCheckin() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）：同渠道多账号绝不批量同刻打上游。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		s.checkinAccount(st, a)
		s.refreshCredits(st, a)
	}
}

// RunCheckinRetries 对退避到期的账号补一次签到（intraday 重试，闸门互斥忙时跳过）。
// 退避公式与持久化见 pool.NoteCheckinRateLimited；成功/已签到清零。
// Source: autumnsentiment/Trae2api-cn @ 2403954 + @ 165ac6e
func (s *Scheduler) RunCheckinRetries() {
	if !s.tryAcquireBatch("trae 签到重试") {
		return
	}
	defer s.batchMu.Unlock()
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !s.cfg.Pool.CheckinRetryDue(st.UID) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		s.checkinAccount(st, a)
		s.refreshCredits(st, a)
	}
}

// RetryLoop 每 interval 扫一次到期重试；ctx 取消时返回。
func (s *Scheduler) RetryLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunCheckinRetries()
		}
	}
}

// RunCreditsNow 立即对所有账号拉取一次积分权益包明细（EntPacks，闸门互斥忙时拒绝）。
// 与签到路径共享 refreshCredits：SetCreditExpiry 覆盖内存快照时触发权威余量差
// 校准（pool.calibrateLocked），使 DeductCredits 的换算系数逐步收敛到真实值。
func (s *Scheduler) RunCreditsNow() {
	if !s.tryAcquireBatch("trae 积分刷新") {
		return
	}
	defer s.batchMu.Unlock()
	s.doCredits()
}

// doCredits 积分明细遍历本体（调用方必须已持有 batchMu）。
func (s *Scheduler) doCredits() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）；EntPacks 是轻查询，用 Light 档。
		if !first {
			time.Sleep(stagger.Light())
		}
		first = false
		s.refreshCredits(st, a)
	}
}

// CreditsLoop 积分明细定点刷新循环：启动即刷一次（拉起时尽快恢复面板临期数据
// 与校准样本窗口），随后按 CreditRefreshEvery 周期刷新；ctx 取消时返回。
// 调用与扣减的闭环：每笔成功调用在内存按倍率×tokens 预扣（pool.DeductCredits，
// FEFO 逐包扣减、24h/48h 到期列即时下降），本循环定期用上游权威余量校准系数。
func (s *Scheduler) CreditsLoop(ctx context.Context) {
	interval := s.cfg.CreditRefreshEvery
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	s.RunCreditsNow()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunCreditsNow()
		}
	}
}

// checkinAccount 单账号一次 status→claim。9074 时轮换设备并记录退避
// （status 限流换 ID 重查一次，claim 限流等退避到期）；成功/已签到清零退避。
func (s *Scheduler) checkinAccount(st pool.Status, a *auth.Auth) {
	identity := upstream.CheckinIdentity(a)
	deviceID, pinned := upstream.CheckinDevice(identity, s.cfg.Pool.CheckinGeneration(st.UID))
	checkedIn, _, enable, err := s.statusWithRefresh(a, deviceID)
	if errors.Is(err, upstream.ErrCheckinRateLimited) && !pinned {
		gen := s.cfg.Pool.BumpCheckinGeneration(st.UID)
		deviceID, _ = upstream.CheckinDevice(identity, gen)
		log.Printf("[trae] checkin status %s: rate limited (9074), rotated device to gen %d", st.UID, gen)
		checkedIn, _, enable, err = s.statusWithRefresh(a, deviceID)
	}
	if err != nil {
		log.Printf("[trae] checkin status %s: %v", st.UID, err)
		return
	}
	if checkedIn {
		s.cfg.Pool.ClearCheckinRetry(st.UID)
		log.Printf("[trae] checkin %s: already checked in", st.UID)
		return
	}
	if !enable {
		return
	}
	if err := s.cfg.Upstream.CheckinClaim(a, deviceID); err != nil {
		if errors.Is(err, upstream.ErrCheckinRateLimited) {
			if !pinned {
				newGen := s.cfg.Pool.BumpCheckinGeneration(st.UID)
				log.Printf("[trae] checkin claim %s: rate limited (9074), rotated device to gen %d", st.UID, newGen)
			}
			after := s.cfg.Pool.NoteCheckinRateLimited(st.UID)
			log.Printf("[trae] checkin claim %s: retry after %s", st.UID, after.Format("15:04:05"))
		} else {
			log.Printf("[trae] checkin claim %s: %v", st.UID, err)
		}
		return
	}
	s.cfg.Pool.ClearCheckinRetry(st.UID)
	log.Printf("[trae] checkin %s: ok", st.UID)
}

// statusWithRefresh 查签到状态；401/1001 认证失败时刷新 token 落盘后只重试一次。
// Source: autumnsentiment/Trae2api-cn @ 87a510a（失败刷新重试一次）。
// 用 RefreshToken（无条件换新；吊销的 token 不会触发 NeedsRefresh），
// 持锁串行，调用方不得并发对同一账号调此函数。
func (s *Scheduler) statusWithRefresh(a *auth.Auth, deviceID string) (bool, int64, bool, error) {
	checkedIn, credits, enable, err := s.cfg.Upstream.CheckinStatus(a, deviceID)
	if err == nil || !isCheckinAuthFailure(err) {
		return checkedIn, credits, enable, err
	}
	uid := a.UID
	if rerr := s.cfg.Upstream.RefreshToken(a); rerr != nil {
		return false, 0, false, err
	}
	if serr := a.SaveAtomic(); serr != nil {
		log.Printf("[trae] checkin %s refresh save: %v", uid, serr)
	}
	return s.cfg.Upstream.CheckinStatus(a, deviceID)
}

// isCheckinAuthFailure 报告签到错误是否为认证失效（HTTP 401 会话失效，
// 或业务码 1001）。其他业务码（9074/9004/9095 等）不是。
func isCheckinAuthFailure(err error) bool {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		return true
	}
	return strings.Contains(err.Error(), "1001")
}

// refreshCredits 查积分明细 + 解冻（有积分的冷却账号恢复）+ 记录最早积分到期时刻。
// 明细来自 ide_user_ent_usage 权益包列表（EntPacks）：remain 聚合值驱动解冻，
// 各包 expire_time 的最小值写进池（跨渠道「谁先到期用谁」排序的真实数据源）。
func (s *Scheduler) refreshCredits(st pool.Status, a *auth.Auth) {
	packs, err := s.cfg.Upstream.EntPacks(a)
	if err != nil {
		log.Printf("[trae] ent-usage %s: %v", st.UID, err)
		return
	}
	remain := int64(0)
	earliest := int64(0)
	var packsSnapshot []pool.CreditPack
	for _, p := range packs {
		remain += p.Limit - int64(p.Used)
		if p.ExpireAt > 0 && (earliest == 0 || p.ExpireAt < earliest) {
			earliest = p.ExpireAt
		}
		packsSnapshot = append(packsSnapshot, pool.CreditPack{ExpireAt: p.ExpireAt, Remain: float64(p.Limit) - p.Used})
	}
	s.cfg.Pool.ReenableIfCredits(st.UID, remain)
	s.cfg.Pool.SetCreditExpiry(st.UID, earliest, packsSnapshot)
}

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用（闸门互斥忙时拒绝）。
func (s *Scheduler) RunRefreshNow() {
	if !s.tryAcquireBatch("trae token 刷新") {
		return
	}
	defer s.batchMu.Unlock()
	s.doRefresh()
}

// doRefresh token 预刷新遍历本体（调用方必须已持有 batchMu）。
func (s *Scheduler) doRefresh() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）：同一秒批量刷新 token 正是风控诱因。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		// 持锁内重查，避免与签到路径的刷新并发重复 ExchangeToken。
		// 注意：失败时返回 (false, err)，必须先判 err 再判 refreshed。
		refreshed, err := s.cfg.Upstream.RefreshTokenIfNeeded(a, s.cfg.RefreshSkew)
		if err != nil {
			log.Printf("[trae] refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		if !refreshed {
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("[trae] refresh %s save: %v", st.UID, err)
		}
	}
}
