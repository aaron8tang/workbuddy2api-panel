// Package pool trae 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者（SPEC §4.7）+ 三因子加权与防惊群。
// 与 workbuddy 主池（internal/pool）完全独立：独立状态文件、独立选号、独立冷却。
package pool

import (
	"encoding/json"
	"log"
	"math"
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
	// CreditPacks 权益包到期明细（面板「24h/48h 到期」列数据源）：每包
	// 到期时刻（Unix 秒）+ 剩余积分。随积分刷新更新并持久化；空 = 未刷新过。
	CreditPacks []CreditPack `json:"credit_packs,omitempty"`
}

// CreditPack 单个权益包的到期明细快照（面板窗口到期量统计用）。
type CreditPack struct {
	ExpireAt int64   `json:"expire_at"`          // Unix 秒；0 = 无限期（不参与到期统计）
	Remain   float64 `json:"remain"`             // 剩余积分（limit - used）
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

	creditExpiry int64         // 最早积分到期时刻（Unix 秒，0=未知）；随积分刷新更新并持久化
	creditPacks  []CreditPack  // 权益包到期明细快照（面板 24h/48h 到期列）
	orderRank    int           // 手工使用顺序名次（1 起，0 = 未排序垫底）
	rateTokens   float64       // 上次权威同步以来累计的 rate 加权 tokens（Σ rate×tokens），校准样本量
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

// earliestPackExpiry 返回全量未到期权益包的最早失效时刻（Unix 秒，0=无未来批次）。
// 选号 FEFO 排序键：不再受 24h 天粒度窗口限制——某批次积分的具体到期时分秒
// （签到 100/150 积分包的 expire_time，上游按 DeductionEnd 精确到秒下发）就是
// 最真实的紧迫度，「谁先失效先用谁」按全量批次比较。已过期（ExpireAt <= now）
// 的陈旧包跳过：积分已作废，不参与未来紧迫度排序；Remain<=0 的包（上游拉取
// 清洗 / 内存 FEFO 扣穿后剔除）同理跳过。
func (e *entry) earliestPackExpiry(now time.Time) int64 {
	nowUnix := now.Unix()
	var earliest int64
	for _, pk := range e.creditPacks {
		if pk.ExpireAt <= nowUnix || pk.Remain <= 0 {
			continue
		}
		if earliest == 0 || pk.ExpireAt < earliest {
			earliest = pk.ExpireAt
		}
	}
	return earliest
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
	// CreditPacks 权益包到期明细快照（面板 24h/48h 到期列数据源），重启不丢。
	CreditPacks []CreditPack `json:"credit_packs,omitempty"`
	// AccountOrder 每账号在 trae 池手工使用顺序中的名次（1 起，0 = 未排序）。
	// 挂在每个账号上持久化，比全局数组对 state.json 的 map 结构更自然。
	AccountOrder int `json:"account_order,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateEntry `json:"accounts"`
	// CalibCreditPerMTok 池级积分换算系数（每百万 tokens × 倍率=1 消耗的积分），
	// 由权威余量差 EMA 校准；0 = 未持久化（回落默认值）。
	CalibCreditPerMTok float64 `json:"calib_credit_per_mtok,omitempty"`
}

// Pool 账号池。
type Pool struct {
	mu          sync.RWMutex
	byUID       map[string]*entry
	stateFp     string
	maxInFlight int
	// calib 积分换算系数：每百万 tokens × 倍率=1 消耗的积分数。上游未公开
	// tokens→积分公式，初始取实测量级起点，随后由 SetCreditExpiry 的权威余量差
	// EMA 自动校准（见 calibrateLocked）；持久化于 state.json 池级字段。
	calib float64
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:       map[string]*entry{},
		stateFp:     stateFp,
		maxInFlight: 3,
		calib:       creditCalibDefault,
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

// Pick 返回 healthy 中的择优账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非候选全部刚被
// 用过，LRU 兜底）。var 而非 const：测试用 withNoPickGap 置 0（与主池同约定）。
var minPickGap = 100 * time.Millisecond

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
// 手工使用顺序生效时（存在 orderRank>0 的账号），名次成为主序：rank 升序、
// 未排序账号垫底，且候选名单截 1——顺序第一名不可用（冷却/在途满/不健康）时
// 根本进不了候选，自然轮到下一名；此时临期硬偏好整体让位（手工顺序压过临期，
// 与主池粘性分配路径 realm.go 同约定：用户手工拖到第一位的远期号必须可被选中）。
// 自动模式下候选中存在未来批次积分的账号时（earliestPackExpiry>0），收敛候选集
// 到这些账号（硬偏好，「先用掉要过期的积分」，积分最高但远期到期的账号中签率为
// 0），排序次键为 FEFO（全量未到期包最早失效时刻升序，精确到时分秒，谁先到期
// 先用谁）。全员无未来批次时与既有三因子行为完全一致。
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

	type cand struct {
		e    *entry
		fefe int64 // 全量未到期权益包的最早失效时刻（Unix 秒，0=无未来批次）
	}
	cs := make([]cand, 0, len(cands))
	nExpiring := 0
	manualOrder := false
	for _, e := range cands {
		if e.orderRank > 0 {
			manualOrder = true
		}
		earliest := e.earliestPackExpiry(now)
		if earliest > 0 {
			nExpiring++
		}
		cs = append(cs, cand{e: e, fefe: earliest})
	}
	// 临期硬偏好只在自动模式生效：手工使用顺序压过它——若硬过滤先剔除远期号，
	// 用户手工拖到第一位的远期号将永远轮不到。幸存集为真子集时才收敛；全员临期
	// 或全员无临期时候选集不变（前者 FEFO 次键仍决定先后，后者零回归）。
	if !manualOrder && nExpiring > 0 && nExpiring < len(cs) {
		log.Printf("[trae_pool] expiring hard tier kept=%d/%d candidates", nExpiring, len(cs))
		kept := make([]cand, 0, nExpiring)
		for _, c := range cs {
			if c.fefe > 0 {
				kept = append(kept, c)
			}
		}
		cs = kept
	}

	var maxCredits int64
	for _, c := range cs {
		if c.e.credits > maxCredits {
			maxCredits = c.e.credits
		}
	}

	type weighted struct {
		e    *entry
		w    float64
		rank int
		fefe int64
	}
	ws := make([]weighted, len(cs))
	for i, c := range cs {
		ws[i] = weighted{e: c.e, w: p.weightOf(c.e, maxCredits, now), rank: c.e.orderRank, fefe: c.fefe}
	}
	sort.Slice(ws, func(i, j int) bool {
		// 手工顺序主序：双方均有名次时 rank 升序；仅一方有名次则其在前。
		ri, rj := ws[i].rank, ws[j].rank
		if ri > 0 && rj > 0 && ri != rj {
			return ri < rj
		}
		if ri > 0 && rj == 0 {
			return true
		}
		if ri == 0 && rj > 0 {
			return false
		}
		// 临期 FEFO 次键：谁先到期先用谁。无临期积分的账号 fefe=0 垫底；全员
		// 无临期时该分支恒假，退化为三因子权重（零回归）。
		if ws[i].fefe != ws[j].fefe {
			return ws[i].fefe < ws[j].fefe
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	top := make([]*entry, 0, 5)
	topN := 5
	// 手工顺序模式截 1：minPickGap 防撞号与 LRU 兜底机制照常生效，
	// 100ms 窗口外的每个请求严格命中顺序第一名。
	if ws[0].rank > 0 {
		topN = 1
	}
	for i := 0; i < len(ws) && i < topN; i++ {
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

// PickExcluding 返回健康且未被排除的账号，走三因子加权与防惊群短名单调度
//（手工顺序主序 + 临期 FEFO 次键 + 权重）。会话粘性已整体移除：prompt_cache_key
// 统一后同会话跨账号/跨池 cache key 恒定，粘性不再有缓存收益，反而把请求钉死在
// 临期账号上（chenwei 实案）——每次调用即时择优，积分最临期者先用。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	best := p.pickBestCandidateLocked(tried, time.Now())
	if best == nil {
		return nil
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
// 手工使用顺序生效时名次压过 work_credits 权重（与主通道语义一致）。
func (p *Pool) PickWorkExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	bestRank := 0
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthyWork(now) {
			continue
		}
		r := e.orderRank
		if best == nil {
			best, bestRank = e, r
			continue
		}
		swap := false
		switch {
		case r > 0 && bestRank == 0:
			swap = true
		case r > 0 && bestRank > 0:
			swap = r < bestRank
		case r == 0 && bestRank == 0:
			swap = e.workCredits > best.workCredits || (e.workCredits == best.workCredits && uid < best.a.UID)
		default:
			// 未排序账号不得挤掉已排序账号（与 PickExcluding 同修复，防 map 遍历抖动）
			swap = false
		}
		if swap {
			best, bestRank = e, r
		}
	}
	if best == nil {
		return nil
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

// SetCreditExpiry 更新账号的最早积分到期时刻与权益包到期明细快照（at=0 清除）。
// 数据源：ide_user_ent_usage 权益包 expire_time 明细（scheduler 积分刷新时写入），
// 供模型组跨渠道「谁先到期用谁」排序与面板「24h/48h 到期」列。
// 覆盖前先做估算校准（calibrateLocked）：本次为上游权威余量，与扣减后的内存
// 余量之差即真实消费，用于修正 DeductCredits 的换算系数。
func (p *Pool) SetCreditExpiry(uid string, at int64, packs []CreditPack) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	changed := p.calibrateLocked(e, packs)
	e.creditExpiry = at
	e.creditPacks = packs
	if changed {
		p.saveLocked()
	}
}

// creditCalibDefault 积分换算系数起点：每百万 tokens × 倍率=1 消耗的积分数。
// 上游未公开公式，取实测量级起点（签到包 150 积分/日 ≈ glm-5.3 0.78× 数百万
// tokens）；持久化后由权威余量差 EMA 收敛到真实值，起点误差只影响首个刷新周期。
const creditCalibDefault = 100.0

// DeductCredits 调用成功后按「倍率 × tokens」在内存中预扣积分：权益包明细按
// FEFO（最早到期优先）扣减、聚合余额同步下降——面板 24h/48h 到期列与 pick()
// 加权轮换下一次调用即反映真实消耗，无需等下一次积分刷新。
// rate 为模型计费倍率（Gateway.RateOf），0 = 未知倍率：直接跳过本次预扣与样本
// 累计（换算瞎猜会污染面板数字；该调用消耗会体现在下次权威校准的余量差中，
// 系数偏差由 EMA 收敛）。totalTokens 取上游 usage.total_tokens。
func (p *Pool) DeductCredits(uid string, rate float64, totalTokens int64) {
	if totalTokens <= 0 || rate <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || p.calib <= 0 {
		return
	}
	cost := p.calib * rate * float64(totalTokens) / 1e6
	if cost <= 0 {
		return
	}
	e.rateTokens += rate * float64(totalTokens)
	deductFEFO(e.creditPacks, cost)
	if e.credits > 0 {
		if c := math.Round(float64(e.credits) - cost); c > 0 {
			e.credits = int64(c)
		} else {
			e.credits = 0
		}
	}
	// 观测日志：flash 类低倍率模型单次消耗远小于 1 积分（int64 余额无感），链路
	// 是否活着只能靠此行确认；cost 累计误差由下次权威刷新的 EMA 校准收敛。
	log.Printf("[trae_pool] deduct %s: rate=%.2f tokens=%d cost=%.4f credits=%d", uid, rate, totalTokens, cost, e.credits)
	p.saveLocked()
}

// deductFEFO 从权益包明细按最早到期优先扣减 cost 积分（ExpireAt=0 视为最远期）。
// 余量扣到 0 为止；明细为空或 cost 超出总余量时尽力扣（聚合余额已同步下降，
// 差额由下次权威刷新校平）。
func deductFEFO(packs []CreditPack, cost float64) {
	if len(packs) == 0 || cost <= 0 {
		return
	}
	idx := make([]int, len(packs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ea, eb := packs[idx[a]].ExpireAt, packs[idx[b]].ExpireAt
		if ea == 0 {
			ea = math.MaxInt64
		}
		if eb == 0 {
			eb = math.MaxInt64
		}
		return ea < eb
	})
	for _, i := range idx {
		if cost <= 0 {
			break
		}
		pk := &packs[i]
		if pk.Remain <= 0 {
			continue
		}
		take := pk.Remain
		if take > cost {
			take = cost
		}
		pk.Remain -= take
		cost -= take
	}
}

// calibClampLo / calibClampHi 换算系数的有界区间：防离群观测把系数推到荒谬值
// （obs→0 时扣减失效，obs→∞ 时一笔扣穿）。正常量级在 1~1000 之间。
const (
	calibClampLo = 1e-3
	calibClampHi = 1e5
)

// calibMinSample 校准最小样本量（rate 加权 tokens）：低于此值视为单请求噪声，
// 不构成样本（如仅一次 5 万 tokens 的 flash 调用，扣减量级太小读不准）。
const calibMinSample = 1e5

// calibrateLocked 权威余量差校准：obs = (扣减后内存余量 − 上游权威余量) /
// (rate 加权百万 tokens)，即每「百万 tokens × 倍率=1」的真实积分消耗。EMA 0.5
// 平滑写入池级系数；返回是否发生系数变更（调用方据此决定落盘）。
// 样本窗口以两次权威同步为界：无论是否成样，rateTokens 均清零（样本必须先读后清）。
// 跳过校准的情形：样本不足（rateTokens < calibMinSample）、期间有权益包到期
// 清退（余量差混入到期作废，非纯消费）、无消费或赠送回补（delta<=0）、观测异常。
func (p *Pool) calibrateLocked(e *entry, packs []CreditPack) bool {
	sample := e.rateTokens // 先读：两次权威同步间累计的 Σ rate×tokens
	e.rateTokens = 0       // 后清：本次权威同步即重置样本窗口
	if len(e.creditPacks) == 0 || len(packs) == 0 {
		return false
	}
	now := time.Now().Unix()
	for _, pk := range e.creditPacks {
		if pk.Remain > 0 && pk.ExpireAt > 0 && pk.ExpireAt <= now {
			return false // 有包在两次同步间到期清退，余量差不纯是消费
		}
	}
	prev, next := 0.0, 0.0
	for _, pk := range e.creditPacks {
		prev += pk.Remain
	}
	for _, pk := range packs {
		next += pk.Remain
	}
	delta := prev - next
	if delta <= 0 {
		return false // 无消费（或签到赠送/回补），不构成样本
	}
	if sample < calibMinSample {
		return false // 样本不足，防单请求噪声
	}
	obs := delta / (sample / 1e6)
	if math.IsNaN(obs) || math.IsInf(obs, 0) || obs <= 0 || obs > calibClampHi {
		log.Printf("[trae_pool] credit calib skipped: obs=%.4f out of range (delta=%.2f, sample=%.0f)", obs, delta, sample)
		return false
	}
	old := p.calib
	p.calib = 0.5*old + 0.5*obs
	if p.calib < calibClampLo {
		p.calib = calibClampLo
	} else if p.calib > calibClampHi {
		p.calib = calibClampHi
	}
	log.Printf("[trae_pool] credit calib: obs=%.4f calib %.4f→%.4f (delta=%.2f credits, sample=%.0f rateTokens)",
		obs, old, p.calib, delta, sample)
	return true
}

// CreditCalib 返回当前换算系数（观测/诊断用）。
func (p *Pool) CreditCalib() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.calib
}

// SetAccountOrder 设置手工使用顺序（uid 有序列表；空 = 清空恢复自动模式）。
// 顺序模式 pickBestCandidateLocked 按 rank 优先选号（显式 override，压过积分权重），
// 未覆盖的账号垫底（rank=0）。每账号名次挂在其 state 条目上持久化。
func (p *Pool) SetAccountOrder(order []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	rank := 0
	for _, uid := range order {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		rank++
		if e, ok := p.byUID[uid]; ok {
			e.orderRank = rank
		}
	}
	for uid, e := range p.byUID {
		if !seen[uid] {
			e.orderRank = 0 // 未覆盖/清空 → 回落自动模式
		}
	}
	p.saveLocked()
}

// AccountOrder 返回当前手工顺序（按 rank 升序的 uid 列表；未启用返回空）。
func (p *Pool) AccountOrder() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	type pair struct {
		uid  string
		rank int
	}
	var ps []pair
	for uid, e := range p.byUID {
		if e.orderRank > 0 {
			ps = append(ps, pair{uid, e.orderRank})
		}
	}
	if len(ps) == 0 {
		return nil
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].rank < ps[j].rank })
	out := make([]string, len(ps))
	for i, v := range ps {
		out[i] = v.uid
	}
	return out
}

// AccountOrderActive 手工使用顺序模式是否生效。
func (p *Pool) AccountOrderActive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byUID {
		if e.orderRank > 0 {
			return true
		}
	}
	return false
}

// orderRankOf 返回 uid 的手工顺序名次（0 = 未排序）。
func (p *Pool) orderRankOf(uid string) int {
	if e, ok := p.byUID[uid]; ok {
		return e.orderRank
	}
	return 0
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
		CreditPacks:          e.creditPacks,
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
	if sf.CalibCreditPerMTok > 0 && sf.CalibCreditPerMTok >= calibClampLo && sf.CalibCreditPerMTok <= calibClampHi {
		p.calib = sf.CalibCreditPerMTok // 池级系数持久化：重启不丢校准结果
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
			creditPacks:       s.CreditPacks,
			orderRank:         s.AccountOrder,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{
		Accounts:           map[string]stateEntry{},
		CalibCreditPerMTok: p.calib,
	}
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
			CreditPacks:       e.creditPacks,
			AccountOrder:      e.orderRank,
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
