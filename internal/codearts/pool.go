// pool.go CodeArts 账号池：选号轮转 + 单飞续期（refresh token 一次性轮换，
// 必须写回）+ 会话心跳闸（上游每账号 3 路并发）。
package codearts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// entry 池内一条。
type entry struct {
	cred     *Credential
	cooling  bool
	until    time.Time
	reason   string
	disabled bool
	errCount int
}

// Pool 账号池（内部 Map；对外状态经 Status 视图）。
type Pool struct {
	mu    sync.Mutex
	byUID map[string]*entry
}

// NewPool 构建账号池。
func NewPool() *Pool { return &Pool{byUID: map[string]*entry{}} }

// SyncToDir 对齐目录：新账号加入、消失的剔除（冷却/禁用状态按 UID 保留）。
func (p *Pool) SyncToDir(creds []*Credential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, cr := range creds {
		uid := cr.SafeUID()
		seen[uid] = true
		if e, ok := p.byUID[uid]; ok {
			e.cred = cr
			continue
		}
		p.byUID[uid] = &entry{cred: cr}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Remove 移除账号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
}

// Get 取账号凭据（未找到 nil）。
func (p *Pool) Get(uid string) *Credential {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		return e.cred
	}
	return nil
}

// UIDs 全部账号（排序稳定）。
func (p *Pool) UIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

// healthy 未禁用且未冷却。
func (e *entry) healthy(now time.Time) bool {
	return !e.disabled && (e.until.IsZero() || now.After(e.until))
}

// Pick 取一个健康账号（跳过 tried；连败计数低者优先）。
func (p *Pool) Pick(tried map[string]bool) (string, *Credential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var bestUID string
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.errCount < best.errCount {
			bestUID, best = uid, e
		}
	}
	if best == nil {
		return "", nil
	}
	return bestUID, best.cred
}

// Cooldown 冷却。
func (p *Pool) Cooldown(uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
}

// Disable 永久禁用（需重新登录）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
}

// Enable 解禁。
func (p *Pool) Enable(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = false
		e.reason = ""
		e.errCount = 0
		e.until = time.Time{}
	}
}

// ClearCooldown 清冷却。
func (p *Pool) ClearCooldown(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Time{}
		e.reason = ""
		e.errCount = 0
	}
}

// NoteError 连败计数；达阈值自动冷却。
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
}

// NoteSuccess 清零连败。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 单账号对外视图。
type Status struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname,omitempty"`
	UserName   string `json:"user_name,omitempty"`
	Cooling    bool   `json:"cooling"`
	Until      string `json:"until,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Disabled   bool   `json:"disabled"`
	ErrCount   int    `json:"err_count,omitempty"`
	HasRefresh bool   `json:"has_refresh"`
	// ExpiresAtMs 控制面临期（毫秒；0 = 未知/解析失败）。
	ExpiresAtMs int64 `json:"expires_at_ms,omitempty"`
}

// List 全部账号状态。
func (p *Pool) List() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		e := p.byUID[uid]
		st := Status{
			UID: uid, Nickname: e.cred.UserName, UserName: e.cred.UserName,
			Cooling: !e.until.IsZero() && now.Before(e.until),
			Reason:  e.reason, Disabled: e.disabled, ErrCount: e.errCount,
			HasRefresh: e.cred.CanRefresh(),
		}
		if !e.until.IsZero() {
			st.Until = e.until.Format(time.RFC3339)
		}
		if ms, ok := e.cred.ExpiresAtMs(); ok {
			st.ExpiresAtMs = ms
		}
		out = append(out, st)
	}
	return out
}

// ---------------------------------------------------------------------------
// 单飞续期
// ---------------------------------------------------------------------------

type flight struct {
	done chan struct{}
	cred *Credential
	err  error
}

// refresher 续期单飞表（key = uid + token 指纹；换 token 换格）。
type refresher struct {
	mu      sync.Mutex
	flights map[string]*flight
}

// RefreshSingleFlight 无条件换一次（先到者发请求，后来者复用同一结果）。
// 成功后写回凭证文件（refresh token 一次性：刷了不存 = 账号死刑）。
func (g *Gateway) RefreshSingleFlight(uid string, cred *Credential, force bool) (*Credential, error) {
	if !force && !cred.NeedsRefresh(RefreshLead, time.Now()) {
		return cred, nil
	}
	if !cred.CanRefresh() {
		return nil, fmt.Errorf("CodeArts 凭据缺少 refresh token 或 OAuth 上下文，无法续期，请重新登录")
	}
	sum := sha256.Sum256([]byte(cred.RefreshToken))
	key := uid + ":" + hex.EncodeToString(sum[:8])
	g.rf.mu.Lock()
	if f, ok := g.rf.flights[key]; ok {
		g.rf.mu.Unlock()
		<-f.done
		return f.cred, f.err
	}
	f := &flight{done: make(chan struct{})}
	g.rf.flights[key] = f
	g.rf.mu.Unlock()
	defer func() {
		g.rf.mu.Lock()
		delete(g.rf.flights, key)
		g.rf.mu.Unlock()
		close(f.done)
	}()
	fresh, err := RefreshCredential(cred)
	if err != nil {
		// 旧凭据还没过期 → 返回旧凭据继续用；已过期才报错。
		if expiry, ok := cred.ExpiresAtMs(); ok && expiry > time.Now().UnixMilli() {
			logf("提前续期失败（%v），旧凭据尚未过期，本次继续使用", err)
			f.cred, f.err = cred, nil
			return cred, nil
		}
		f.err = err
		return nil, err
	}
	fresh.FilePath = cred.FilePath
	if g.authDir != "" {
		if err := fresh.SaveAtomic(g.authDir); err != nil {
			logf("续期后写回凭证失败（refresh token 已轮换，不落盘将丢失）: %v", err)
		}
	}
	g.pool.mu.Lock()
	if e, ok := g.pool.byUID[uid]; ok {
		e.cred = fresh
	}
	g.pool.mu.Unlock()
	f.cred = fresh
	return fresh, nil
}

// ---------------------------------------------------------------------------
// 会话闸（本地准入 + 上游心跳）
// ---------------------------------------------------------------------------

// sessionGate 同一上游身份的并发会话闸（上游硬顶 3 路；满员 409 而非 429）。
type sessionGate struct {
	mu    sync.Mutex
	limit int
	used  map[string]int
}

// SessionHandle 一次在途会话（idle 注销由调用方完成）。
type SessionHandle struct {
	g    *sessionGate
	key  string
	id   string
}

func (g *sessionGate) acquire(baseURL, identity string) (*SessionHandle, error) {
	sum := sha256.Sum256([]byte(trimRightSlash(baseURL) + "\n" + identity))
	key := hex.EncodeToString(sum[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.used[key] >= g.limit {
		return nil, fmt.Errorf("CodeArts 账号并发会话已达上限（%d），等在途请求结束后重试", g.limit)
	}
	g.used[key]++
	return &SessionHandle{g: g, key: key, id: uuidV4Hex()}, nil
}

func (h *SessionHandle) release() {
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	if h.g.used[h.key] <= 1 {
		delete(h.g.used, h.key)
	} else {
		h.g.used[h.key]--
	}
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
