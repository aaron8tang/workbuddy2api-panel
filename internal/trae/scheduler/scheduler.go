// Package scheduler trae 子系统定时任务：每日签到 + token 预刷新 + 9074 退避重试。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
// 与 workbuddy 主调度器（internal/scheduler）完全独立：独立池、独立上游、独立排程。
package scheduler

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

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
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if s.cfg.CheckinHour == h {
				s.RunCheckinNow()
			}
		}
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

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		s.checkinAccount(st, a)
		s.refreshCredits(st, a)
	}
}

// RunCheckinRetries 对退避到期的账号补一次签到（intraday 重试）。
// 退避公式与持久化见 pool.NoteCheckinRateLimited；成功/已签到清零。
// Source: autumnsentiment/Trae2api-cn @ 2403954 + @ 165ac6e
func (s *Scheduler) RunCheckinRetries() {
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

// RunCreditsNow 立即对所有账号拉取一次积分权益包明细（EntPacks）。
// 与签到路径共享 refreshCredits：SetCreditExpiry 覆盖内存快照时触发权威余量差
// 校准（pool.calibrateLocked），使 DeductCredits 的换算系数逐步收敛到真实值。
func (s *Scheduler) RunCreditsNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
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

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用。
func (s *Scheduler) RunRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
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
