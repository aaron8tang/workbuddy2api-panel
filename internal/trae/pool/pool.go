// Package pool trae 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者（SPEC §4.7）+ 三因子加权与防惊群。
// 与 workbuddy 主池（internal/pool）完全独立：独立状态文件、独立选号、独立冷却。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolPlan CoolKind = iota // 1005 plan 权益不足 → 12h 长冷却
	CoolSoft                 // 429/404 → 60s 短冷却（404 不累计 errCount）
	CoolErr                  // 连续错误 → 10m 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolPlan:
		return "plan_limit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏，不含 token）。
type Status struct {
	UID         string    `json:"uid"`
	Nickname    string    `json:"nickname,omitempty"`
	Credits     int64     `json:"credits"`
	WorkCredits float64   `json:"work_credits"`
	Cooling     bool      `json:"cooling"`
	Until       time.Time `json:"until,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	WorkCooling bool      `json:"work_cooling"`
	WorkUntil   time.Time `json:"work_until,omitempty"`
	WorkReason  string    `json:"work_reason,omitempty"`
	// Disabled = session 失效硬禁用（需重登或换文件恢复）；Enabled = 软开关（用户可逆启停）。
	// 对外暴露：Disabled 与 Enabled 都为 false 才算可被 Pick（healthy）。
	Disabled bool `json:"disabled"`
	Enabled  bool `json:"enabled"`
	ErrCount int  `json:"err_count,omitempty"`

	// 运行态指标
	InFlight     int       `json:"in_flight"`
	SuccessCount int64     `json:"success_count,omitempty"`
	ErrTotal     int64     `json:"err_total,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	LastErr      time.Time `json:"last_err,omitempty"`

	// EarliestCreditExpiry 最早积分到期时刻（Unix 秒）：ide_user_ent_usage 权益包
	// 中 status=1 且有剩余积分的包里最早的 expire_time。0 = 未知（尚未刷新过明细）。
	// 供模型组跨渠道「谁先到期用谁」排序使用；签到/积分刷新时更新。
	EarliestCreditExpiry int64 `json:"earliest_credit_expiry,omitempty"`
}

type entry struct {
	a            *auth.Auth
	credits      int64
	workCredits  float64
	disabled     bool // session dead 硬禁用
	enabled      bool // 用户软开关（默认 true），false 时 Pick 跳过
	reason       string
	until        time.Time
	workReason   string
	workUntil    time.Time
	errCount     int
	workErrCount int

	// 9074 签到自愈状态
	checkinGen        int       // 签到设备轮换代数（9074 限流时 +1，上游从不清零）
	checkinRetryAfter time.Time // 9074 退避截止（wall-clock，重启不丢）
	checkinRetryCount int       // 9074 累计次数（决定退避增长；成功/已签到清零）

	// 运行态在途租约与三因子统计
	inFlight     int
	lastUsed     time.Time
	successCount int64
	errTotal     int64
	lastSuccess  time.Time
	lastErr      time.Time

	creditExpiry int64 // 最早积分到期时刻（Unix 秒，0=未知）；随积分刷新更新并持久化
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled || !e.enabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

func (e *entry) healthyWork(now time.Time) bool {
	if e.disabled || !e.enabled {
		return false
	}
	if !e.workUntil.IsZero() && now.Before(e.workUntil) {
		return false
	}
	return true
}

// stateEntry state.json 单账号持久化条目。
type stateEntry struct {
	Credits           int64     `json:"credits"`
	WorkCredits       *float64  `json:"work_credits,omitempty"`
	Disabled          bool      `json:"disabled"`
	Enabled           *bool     `json:"enabled,omitempty"` // 指针：旧文件缺省时按 true 处理，不写回脏值
	Reason            string    `json:"reason,omitempty"`
	Until             time.Time `json:"until,omitempty"`
	WorkReason        string    `json:"work_reason,omitempty"`
	WorkUntil         time.Time `json:"work_until,omitempty"`
	CheckinGen        int       `json:"checkin_device_gen,omitempty"` // 旧文件缺省为 0（基线设备 ID）
	CheckinRetryAfter time.Time `json:"checkin_retry_after,omitempty"`
	CheckinRetryCount int       `json:"checkin_retry_count,omitempty"`
	// CreditExpiry 最早积分到期时刻（Unix 秒，0=未知）。来自权益包 expire_time 明细，
	// 重启不丢（跨渠道到期排序依赖它，避免每次重启都等下一次积分刷新）。
	CreditExpiry int64 `json:"credit_expiry,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateEntry `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu          sync.RWMutex
	byUID       map[string]*entry
	stateFp     string
	affinity    map[string]affinityEntry
	affinityTTL time.Duration
	maxInFlight int
}

type affinityEntry struct {
	uid      string
	lastSeen time.Time
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:       map[string]*entry{},
		stateFp:     stateFp,
		affinity:    map[string]affinityEntry{},
		affinityTTL: 30 * time.Minute,
		maxInFlight: 3,
	}
	if stateFp != "" {
		p.load()
	}
	return p
}

// SetMaxInFlight 设置单账号最大并发在途请求数（<=0 表示不限制）。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxInFlight = n
}

// inFlightFull 报告账号是否已占满在途名额（maxInFlight<=0 时恒 false）。
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight >= p.maxInFlight
}

// Acquire 尝试为指定账号获取在途并发名额（无论是否满载均计数，调用方需注意配合 Release）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.inFlight++
		return true
	}
	return false
}

// Release 释放指定账号的在途并发名额。
func (p *Pool) Release(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.inFlight > 0 {
		e.inFlight--
	}
}

// RecordSuccess 记录指定账号成功调用一次，清零连续错误计数并更新成功统计与最后成功时间。
func (p *Pool) RecordSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.errCount = 0
	}
}

// RecordError 记录指定账号错误一次，累计总错误数。
func (p *Pool) RecordError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
	}
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling/enabled 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a, enabled: true}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a, enabled: true}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Remove 删除账号（仅清内存索引与 state.json 条目；auths/trae-{uid}.json 由调用方删）。
// 不存在返回 false，调用方据此决定 404。
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; !ok {
		return false
	}
	delete(p.byUID, uid)
	p.saveLocked()
	return true
}

// SetEnabled 切换账号软开关；不影响 disabled（session dead）状态。
// reason 仅在关闭时记录。不存在返回 false。
func (p *Pool) SetEnabled(uid string, enabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.enabled = enabled
	if !enabled && reason != "" {
		e.reason = reason
	}
	if enabled {
		// 重新启用时清掉软关闭的 reason；disabled/cooling 不动
		if e.reason != "" && !e.disabled && e.until.IsZero() {
			e.reason = ""
		}
	}
	p.saveLocked()
	return true
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 优化：当积分相同时按 UID 升序稳定排序，避免 Go map 随机遍历导致在相同积分账号间跳跃。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.credits > best.credits || (e.credits == best.credits && uid < best.a.UID) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

const minPickGap = 100 * time.Millisecond

// weightOf 计算三因子调度权重：
// 1. credits 占比 × 10
// 2. 闲置补偿（每闲置 1h 权重 +0.5，上限 5.0）
// 3. 历史成功率 × 3
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	if maxCredits > 0 && e.credits > 0 {
		w += (float64(e.credits) / float64(maxCredits)) * 10.0
	}
	if !e.lastUsed.IsZero() {
		idleH := now.Sub(e.lastUsed).Hours()
		if idleH > 0 {
			idleW := idleH * 0.5
			if idleW > 5.0 {
				idleW = 5.0
			}
			w += idleW
		}
	}
	total := e.successCount + e.errTotal
	if total > 0 {
		sr := float64(e.successCount) / float64(total)
		w += sr * 3.0
	}
	return w
}

// pickBestCandidateLocked 综合健康检查、在途租约、三因子权重与防惊群窗口选号。
func (p *Pool) pickBestCandidateLocked(tried map[string]bool, now time.Time) *entry {
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		cands = append(cands, e)
	}
	// 若全部健康账号均占满在途并发，放宽在途限制
	if len(cands) == 0 {
		for uid, e := range p.byUID {
			if tried != nil && tried[uid] {
				continue
			}
			if e.healthy(now) {
				cands = append(cands, e)
			}
		}
	}
	if len(cands) == 0 {
		return nil
	}

	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}

	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(cands))
	for i, e := range cands {
		ws[i] = weighted{e: e, w: p.weightOf(e, maxCredits, now)}
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	top := make([]*entry, 0, 5)
	for i := 0; i < len(ws) && i < 5; i++ {
		top = append(top, ws[i].e)
	}

	eligible := make([]*entry, 0, len(top))
	for _, e := range top {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var selected *entry
	if len(eligible) == 0 {
		selected = top[0]
		for _, c := range top[1:] {
			if c.lastUsed.Before(selected.lastUsed) {
				selected = c
			}
		}
	} else {
		selected = eligible[0]
	}
	selected.lastUsed = now
	return selected
}

// PickAffinity 优先按 sessionKey 会话粘性返回健康账号；未命中或已冷却则降级择优并绑定，
// 保持同一会话持续使用同一账号，最大化大模型服务端 Prompt/KV Cache 命中率。
func (p *Pool) PickAffinity(sessionKey string, tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	// 1. 若提供了 sessionKey，优先复用健康的已有绑定账号（且未占满在途并发）
	if sessionKey != "" {
		if aff, ok := p.affinity[sessionKey]; ok {
			if tried == nil || !tried[aff.uid] {
				if e, exists := p.byUID[aff.uid]; exists && e.healthy(now) && !p.inFlightFull(e) {
					aff.lastSeen = now
					p.affinity[sessionKey] = aff
					e.lastUsed = now
					return e.a
				}
			}
		}
	}

	// 2. 无粘性绑定或原账号已不可用/已满载，走三因子加权与防惊群短名单调度
	best := p.pickBestCandidateLocked(tried, now)
	if best == nil {
		return nil
	}

	// 3. 记录新的会话粘性绑定
	if sessionKey != "" {
		p.affinity[sessionKey] = affinityEntry{
			uid:      best.a.UID,
			lastSeen: now,
		}
		// 惰性清理过期会话
		if len(p.affinity) > 500 {
			for k, v := range p.affinity {
				if now.Sub(v.lastSeen) > p.affinityTTL {
					delete(p.affinity, k)
				}
			}
		}
	}

	return best.a
}

// PickWork 返回 healthyWork 中 work_credits 最多的账号；无可用返回 nil。
func (p *Pool) PickWork() *auth.Auth {
	return p.PickWorkExcluding(nil)
}

// PickWorkExcluding 返回 healthyWork 中未被排除的账号。
// 调度策略：优先选择可用 work_credits 最多的账号；
// 若所有账号 work_credits 均为 0（如尚未探测），仍选择首个健康账号以供探测与请求。
func (p *Pool) PickWorkExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthyWork(now) {
			continue
		}
		if best == nil || e.workCredits > best.workCredits || (e.workCredits == best.workCredits && uid < best.a.UID) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// PickWorkAffinity 针对 Work 通道实现会话粘性调度。
func (p *Pool) PickWorkAffinity(sessionKey string, tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	if sessionKey != "" {
		workKey := "work:" + sessionKey
		if aff, ok := p.affinity[workKey]; ok {
			if tried == nil || !tried[aff.uid] {
				if e, exists := p.byUID[aff.uid]; exists && e.healthyWork(now) {
					aff.lastSeen = now
					p.affinity[workKey] = aff
					return e.a
				}
			}
		}
	}

	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthyWork(now) {
			continue
		}
		if best == nil || e.workCredits > best.workCredits || (e.workCredits == best.workCredits && uid < best.a.UID) {
			best = e
		}
	}
	if best == nil {
		return nil
	}

	if sessionKey != "" {
		p.affinity["work:"+sessionKey] = affinityEntry{
			uid:      best.a.UID,
			lastSeen: now,
		}
	}

	return best.a
}

// SetWorkCredits 更新账号 Work 通道积分余额。
func (p *Pool) SetWorkCredits(uid string, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.workCredits = credits
		// 若余额充足且因余额耗尽处于冷却中，自动解冻
		if credits > 0 && e.workReason == "work_credits 余额不足" {
			e.workUntil = time.Time{}
			e.workReason = ""
		}
	}
	p.saveLocked()
}

// GetWorkCredits 获取账号 Work 通道积分余额。
func (p *Pool) GetWorkCredits(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.workCredits
	}
	return 0
}

// CooldownWork 冷却账号 Work 通道至 now+d。
func (p *Pool) CooldownWork(uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.workUntil = time.Now().Add(d)
		e.workReason = reason
		e.workErrCount = 0
	}
	p.saveLocked()
}

// NoteWorkError 记录一次 Work 请求错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteWorkError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.workErrCount++
		if e.workErrCount >= threshold {
			e.workUntil = time.Now().Add(d)
			e.workReason = "consecutive work errors"
			e.workErrCount = 0
		}
	}
	p.saveLocked()
}

// NoteWorkSuccess Work 成功请求重置错误计数。
func (p *Pool) NoteWorkSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.workErrCount = 0
	}
}

// SetCredits 更新账号积分。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// SetCreditExpiry 更新账号的最早积分到期时刻（Unix 秒；0 = 清除/未知）。
// 数据源：ide_user_ent_usage 权益包 expire_time 明细（scheduler 积分刷新时写入），
// 供模型组跨渠道「谁先到期用谁」排序。
func (p *Pool) SetCreditExpiry(uid string, at int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.creditExpiry = at
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 失效），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteError 记录一次错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// CheckinGeneration 返回账号的签到设备轮换代数（未知账号或未轮换过为 0）。
func (p *Pool) CheckinGeneration(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok && e.checkinGen > 0 {
		return e.checkinGen
	}
	return 0
}

// BumpCheckinGeneration 将签到设备代数 +1 并持久化（9074 限流后调用）；
// 下次签到即换新设备 ID。不存在返回 0。
func (p *Pool) BumpCheckinGeneration(uid string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0
	}
	e.checkinGen++
	p.saveLocked()
	return e.checkinGen
}

// 9074 退避参数：基线 60s，按次数指数增长并封顶。
// Source: autumnsentiment/Trae2api-cn @ 2403954 + @ 165ac6e
// （base 60s × 2^min(count,3)，上限 3600s；从不碰签到日期，只记退避）。
const (
	checkinRetryBase    = time.Minute
	checkinRetryExpCap  = 3
	checkinRetryMaxBack = time.Hour
)

// checkinBackoff 返回第 count 次 9074 后的等待时长（count 从 0 起）。
func checkinBackoff(count int) time.Duration {
	shift := count
	if shift > checkinRetryExpCap {
		shift = checkinRetryExpCap
	}
	d := checkinRetryBase << shift // 60→120→240→480s
	if d > checkinRetryMaxBack {
		d = checkinRetryMaxBack
	}
	return d
}

// NoteCheckinRateLimited 记录一次 9074，返回下次可重试时刻（wall-clock，持久化）。
func (p *Pool) NoteCheckinRateLimited(uid string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}
	}
	after := time.Now().Add(checkinBackoff(e.checkinRetryCount))
	e.checkinRetryCount++
	e.checkinRetryAfter = after
	p.saveLocked()
	return after
}

// CheckinRetryDue 报告账号是否有到期的签到重试（从未记录退避时为 false，
// 首轮尝试归每日定时任务，避免后台循环与定时任务重复领取）。
func (p *Pool) CheckinRetryDue(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok || e.checkinRetryCount == 0 {
		return false
	}
	return !time.Now().Before(e.checkinRetryAfter)
}

// ClearCheckinRetry 清除 9074 退避（签到成功或已签到时调用；不碰代数）。
func (p *Pool) ClearCheckinRetry(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && (e.checkinRetryCount != 0 || !e.checkinRetryAfter.IsZero()) {
		e.checkinRetryCount = 0
		e.checkinRetryAfter = time.Time{}
		p.saveLocked()
	}
}

// SetCheckinRetryAfterForTest 直接设置退避截止（仅供测试时间旅行；生产请用 NoteCheckinRateLimited）。
func (p *Pool) SetCheckinRetryAfterForTest(uid string, t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.checkinRetryAfter = t
		p.saveLocked()
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	nick := ""
	if e.a != nil {
		nick = e.a.Nickname
	}
	return Status{
		UID:          uid,
		Nickname:     nick,
		Credits:      e.credits,
		WorkCredits:  e.workCredits,
		Cooling:      !e.until.IsZero() && now.Before(e.until),
		Until:        e.until,
		Reason:       e.reason,
		WorkCooling:  !e.workUntil.IsZero() && now.Before(e.workUntil),
		WorkUntil:    e.workUntil,
		WorkReason:   e.workReason,
		Disabled:     e.disabled,
		Enabled:      e.enabled,
		ErrCount:     e.errCount,
		InFlight:     e.inFlight,
		SuccessCount: e.successCount,
		ErrTotal:     e.errTotal,
		LastSuccess:  e.lastSuccess,
		LastErr:      e.lastErr,

		EarliestCreditExpiry: e.creditExpiry,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		enabled := true // 旧文件无 enabled 字段 → 默认启用，向后兼容
		if s.Enabled != nil {
			enabled = *s.Enabled
		}
		wc := 0.0
		if s.WorkCredits != nil {
			wc = *s.WorkCredits
		}
		gen := s.CheckinGen
		if gen < 0 {
			gen = 0
		}
		p.byUID[uid] = &entry{
			a:                 &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:           s.Credits,
			workCredits:       wc,
			disabled:          s.Disabled,
			enabled:           enabled,
			reason:            s.Reason,
			until:             s.Until,
			workReason:        s.WorkReason,
			workUntil:         s.WorkUntil,
			checkinGen:        gen,
			checkinRetryAfter: s.CheckinRetryAfter,
			checkinRetryCount: s.CheckinRetryCount,
			creditExpiry:      s.CreditExpiry,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]stateEntry{}}
	for uid, e := range p.byUID {
		se := stateEntry{
			Credits:           e.credits,
			Disabled:          e.disabled,
			Reason:            e.reason,
			Until:             e.until,
			WorkReason:        e.workReason,
			WorkUntil:         e.workUntil,
			CheckinGen:        e.checkinGen,
			CheckinRetryAfter: e.checkinRetryAfter,
			CheckinRetryCount: e.checkinRetryCount,
			CreditExpiry:      e.creditExpiry,
		}
		if e.workCredits > 0 {
			wc := e.workCredits
			se.WorkCredits = &wc
		}
		// 仅在软关闭时写 enabled=false；默认 true 用 omitempty 省略，旧版本读为 true。
		if !e.enabled {
			f := false
			se.Enabled = &f
		}
		sf.Accounts[uid] = se
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
