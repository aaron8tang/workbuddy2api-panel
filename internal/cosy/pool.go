package cosy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Account 账号池中的一条账号记录。
// 渠道特有的凭证字段（access token / refresh token / 过期时间等）放在 Data 中，
// 池本身只负责 UID、指纹与调度状态。
type Account struct {
	UID         string      `json:"uid"`
	Nickname    string      `json:"nickname,omitempty"`
	Fingerprint Fingerprint `json:"fingerprint"`
	Data        any         `json:"-"`
}

// NewAccount 建立账号；指纹未生成时自动生成一份并落入账号。
func NewAccount(uid, nickname string, fp Fingerprint, data any) *Account {
	if !fp.Complete() {
		fp = NewFingerprint()
	}
	return &Account{UID: uid, Nickname: nickname, Fingerprint: fp, Data: data}
}

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额/额度耗尽 → 长冷却
	CoolSoft                 // 429 软限流 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard"
	case CoolSoft:
		return "soft"
	default:
		return "err"
	}
}

// Status 单账号对外状态（脱敏）。
type Status struct {
	UID       string    `json:"uid"`
	Nickname  string    `json:"nickname,omitempty"`
	Credits   int64     `json:"credits"`
	Cooling   bool      `json:"cooling"`
	Until     time.Time `json:"until,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Disabled  bool      `json:"disabled"`
	ErrCount  int       `json:"err_count,omitempty"`
	Exhausted bool      `json:"exhausted,omitempty"`
}

type poolEntry struct {
	a        *Account
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int
}

func (e *poolEntry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

type acctState struct {
	Credits      int64     `json:"credits"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	MachineID    string    `json:"machine_id,omitempty"`
	MachineToken string    `json:"machine_token,omitempty"`
	MachineType  string    `json:"machine_type,omitempty"`
}

type poolStateFile struct {
	Accounts map[string]acctState `json:"accounts"`
}

// Pool 通用 COSY 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*poolEntry
	stateFp string
}

// New 构建账号池；stateFp 非空时加载历史状态。
func NewPool(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*poolEntry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// StateFile 返回状态文件路径。
func (p *Pool) StateFile() string { return p.stateFp }

// Add 加入账号；已存在时保留冷却/积分状态，并继承已生成的机器指纹。
func (p *Pool) Add(a *Account) {
	if a == nil || a.UID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		if e.a != nil && e.a.Fingerprint.MachineID != "" {
			a.Fingerprint = e.a.Fingerprint
		}
		e.a = a
		return
	}
	if !a.Fingerprint.Complete() {
		a.Fingerprint = NewFingerprint()
	}
	p.byUID[a.UID] = &poolEntry{a: a}
	p.saveLocked()
}

// SyncToDir 对齐目录扫描结果：新账号加入、消失的剔除（状态仍留在 state.json）。
func (p *Pool) SyncToDir(accts []*Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(accts))
	for _, a := range accts {
		if a == nil || a.UID == "" {
			continue
		}
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			if e.a != nil && e.a.Fingerprint.MachineID != "" {
				a.Fingerprint = e.a.Fingerprint
			}
			e.a = a
			continue
		}
		if !a.Fingerprint.Complete() {
			a.Fingerprint = NewFingerprint()
		}
		p.byUID[a.UID] = &poolEntry{a: a}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
	p.saveLocked()
}

// Remove 移除账号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// Pick 返回 healthy 中积分最高的账号。
func (p *Pool) Pick() *Account { return p.PickExcluding(nil) }

// PickExcluding 返回 healthy 中积分最高的账号，跳过 tried 中的 UID。
func (p *Pool) PickExcluding(tried map[string]bool) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *poolEntry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.credits > best.credits {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// Get 按 UID 取账号。
func (p *Pool) Get(uid string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// SetCredits 更新积分。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号。
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

// ClearCooldown 清除冷却。
func (p *Pool) ClearCooldown(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Time{}
		e.reason = ""
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（凭证失效，需重新登录）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// Enable 解除禁用。
func (p *Pool) Enable(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = false
		e.reason = ""
		e.errCount = 0
		e.until = time.Time{}
	}
	p.saveLocked()
}

// ReenableIfCredits 签到/刷新后按剩余积分解冻（仅当 remain>0 且未禁用）。
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

// NoteError 记录错误；达阈值自动冷却。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if threshold > 0 && e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
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

// List 返回全部账号状态（按 UID 排序）。
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

func (p *Pool) statusOf(uid string, e *poolEntry) Status {
	now := time.Now()
	nick := ""
	if e.a != nil {
		nick = e.a.Nickname
	}
	return Status{
		UID:       uid,
		Nickname:  nick,
		Credits:   e.credits,
		Cooling:   !e.until.IsZero() && now.Before(e.until),
		Until:     e.until,
		Reason:    e.reason,
		Disabled:  e.disabled,
		ErrCount:  e.errCount,
		Exhausted: e.credits <= 0,
	}
}

// SaveState 显式持久化（指纹变更后调用）。
func (p *Pool) SaveState() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf poolStateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &poolEntry{
			a: &Account{
				UID: uid,
				Fingerprint: Fingerprint{
					MachineID:    s.MachineID,
					MachineToken: s.MachineToken,
					MachineType:  s.MachineType,
				},
			},
			credits:  s.Credits,
			disabled: s.Disabled,
			reason:   s.Reason,
			until:    s.Until,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := poolStateFile{Accounts: make(map[string]acctState, len(p.byUID))}
	for uid, e := range p.byUID {
		var fp Fingerprint
		if e.a != nil {
			fp = e.a.Fingerprint
		}
		sf.Accounts[uid] = acctState{
			Credits:      e.credits,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			MachineID:    fp.MachineID,
			MachineToken: fp.MachineToken,
			MachineType:  fp.MachineType,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
