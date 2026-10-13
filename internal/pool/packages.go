// 积分包快照：本地保存上游逐包明细（每天拉一次 + 落盘防重启失忆），
// 消耗积分的调用按 FEFO（失效时刻升序）实时扣减包内余额；24h/48h 到期窗口
// 与最早批次由快照派生（statusOf 透出），选号与前端展示不再依赖每次现打
// 上游的明细缓存。包快照独立于聚合值（credits/creditsExpiring 等）生命周期：
// 聚合值仍由余额刷新/签到任务 authoritative 维护，余额刷新不清快照。
package pool

import (
	"sort"
	"time"
)

// PkgSnapshot 单个积分包的本地快照（pool 不 import upstream，由调用方转换）。
// 只保留 FEFO 扣减与到期窗口聚合所需的最小字段：剩余量与失效时刻。
type PkgSnapshot struct {
	Remain   int64     // 包内剩余积分
	ExpireAt time.Time // 失效时刻（上游 DeductionEndTime 优先口径，真「用不完就没了」）
}

// pkgSnapshot entry 内部的包快照（与 PkgSnapshot 同构；运行态与持久化共用单一表示）。
type pkgSnapshot = PkgSnapshot

// SetCreditPackages 用上游逐包明细重写账号的本地包快照（每日拉取任务调用）。
// 清洗口径：
//   - 负余额钳 0、零余额/无到期时间的包剔除（对路由与展示无意义）；
//   - 已过期的包剔除（上游列表按「到期日在未来」过滤过，这里兜底时钟偏移）；
//   - 按 ExpireAt 升序存放——NoteModelCost 的 FEFO 扣减与状态聚合直接按序遍历。
//
// 只重写 pkgs，不动聚合值（credits/creditsExpiring 等由余额刷新维护）。
func (p *Pool) SetCreditPackages(uid string, pkgs []PkgSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	clean := make([]pkgSnapshot, 0, len(pkgs))
	for _, k := range pkgs {
		if k.Remain <= 0 || k.ExpireAt.IsZero() || !now.Before(k.ExpireAt) {
			continue
		}
		clean = append(clean, pkgSnapshot{Remain: k.Remain, ExpireAt: k.ExpireAt})
	}
	sort.SliceStable(clean, func(i, j int) bool { return clean[i].ExpireAt.Before(clean[j].ExpireAt) })
	e.pkgs = clean
	p.dirty.Store(true)
}

// consumePkgsLocked 按失效时刻升序（FEFO，与上游扣包顺序同源）从包快照扣减
// d 点积分，扣空的包删除。返回实际扣减量（受快照总余额钳制）。
// 调用方必须已持有 p.mu 写锁。
func consumePkgsLocked(e *entry, d int64) int64 {
	if d <= 0 || len(e.pkgs) == 0 {
		return 0
	}
	var used int64
	kept := e.pkgs[:0]
	for _, k := range e.pkgs {
		if d <= 0 {
			kept = append(kept, k)
			continue
		}
		if k.Remain <= d {
			d -= k.Remain
			used += k.Remain
			continue // 扣穿整包：不保留
		}
		k.Remain -= d
		used += d
		d = 0
		kept = append(kept, k)
	}
	if len(kept) == 0 {
		e.pkgs = nil
	} else {
		e.pkgs = kept
	}
	return used
}

// pkgsExpiringFor 返回快照中 winHours 窗口内（天粒度）作废的积分合计；无快照返回 ok=false。
// 供 statusOf 透出 exp_24h/exp_48h（前端 24h/48h 到期列的数据源）。
// 天粒度口径与前端 expBatches 一致：批次到期日 d 满足 d >= 0 && d <= winDays
// （winDays = winHours/24，24h→1 天、48h→2 天）计入。
// 已失效的包一律跳过：快照每日才刷新，期间跨过失效时刻的包积分已作废，
// 不能靠日粒度归入"今天"（昨天的包经 int 向零截断也会得出 d=0）。
// remain>0 由写入侧保证（SetCreditPackages/恢复/consumePkgsLocked 均剔除空包）。
func pkgsExpiringFor(pkgs []pkgSnapshot, winHours int, now time.Time) (int64, bool) {
	if len(pkgs) == 0 {
		return 0, false
	}
	winDays := winHours / 24 // 24h→1 天、48h→2 天（与前端 Math.floor(winHours/24) 同口径）
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var sum int64
	for _, k := range pkgs {
		if !now.Before(k.ExpireAt) {
			continue // 已过期的陈旧包：积分已作废，不计入未来窗口
		}
		day := time.Date(k.ExpireAt.Year(), k.ExpireAt.Month(), k.ExpireAt.Day(), 0, 0, 0, 0, k.ExpireAt.Location())
		dDays := int(day.Sub(midnight).Hours() + 0.5) / 24
		if dDays >= 0 && dDays <= winDays {
			sum += k.Remain
		}
	}
	return sum, true
}

// CountExpiring24h 统计健康账号中存在 24h 内（天粒度）到期积分的账号数。
// 健康口径与 EarliestCreditExpiry 一致（e.healthy：非禁用/暂停/冷却）；"有
// 24h 内到期积分" 基于 pkgs 快照（含消耗实时扣减的最新数据），值为 >0 才算。
// realm 非空时只统计该 realm（cn/global）的账号。供模型组路由判断「主池多个
// 账号都有快到期积分时优先用 trae 渠道」（跨渠道 boost 键）。
func (p *Pool) CountExpiring24h(realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if v, ok := pkgsExpiringFor(e.pkgs, 24, now); ok && v > 0 {
			n++
		}
	}
	return n
}

// pkgsEarliestDay 返回快照中最早未到期批次的本地日期（"2006-01-02"）；无未到期
// 批次返回 false。供 statusOf 透出 earliest_expiry_day（前端 FEFO 展示排序的数据源）。
// 跳过已失效的陈旧包（每日刷新前跨过失效时刻的），避免透出过去日期。
func pkgsEarliestDay(pkgs []pkgSnapshot, now time.Time) (string, bool) {
	for _, k := range pkgs {
		if now.Before(k.ExpireAt) {
			return k.ExpireAt.Format("2006-01-02"), true
		}
	}
	return "", false
}
