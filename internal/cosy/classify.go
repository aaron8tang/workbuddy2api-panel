package cosy

import (
	"errors"
	"fmt"
	"strings"
)

// ErrKind 错误类别，驱动账号池冷却状态机。
type ErrKind int

const (
	ErrNone         ErrKind = iota
	ErrHardCredit           // 余额/额度耗尽（402 或关键词）
	ErrSoftRate             // 429 软限流
	ErrBusy                 // 上游忙（business code 10605 等）→ 可按指定等待重试
	ErrTokenExpired         // dt 过期（401 TOKEN_EXPIRE）→ 触发 refresh 重试
	ErrSessionDead          // 凭证失效 → 禁用（需重新登录）
	ErrNotFound             // 404 上游偶发 → 短冷却不累计 errCount（防雪崩）
	ErrServer               // 5xx
	ErrClient               // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrBusy:
		return "busy"
	case ErrTokenExpired:
		return "token_expired"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 让 ErrKind 可作 errors.Is 的 target。
func (k ErrKind) Error() string { return k.String() }

// Error 带分类的上游错误。
type Error struct {
	Kind     ErrKind
	Status   int
	Msg      string
	RetryAt  int64 // 上游给出的可重试时刻（unix 秒，0 表示未知）
	Deadline bool  // 是否携带明确的额度/限额截止时刻
}

func (e *Error) Error() string {
	return fmt.Sprintf("cosy %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Is 让 errors.Is(err, ErrKind) 可用。
func (e *Error) Is(target error) bool {
	if k, ok := target.(ErrKind); ok {
		return e.Kind == k
	}
	return false
}

// hardMarkers 命中任一即判定为额度耗尽（中英双语，覆盖两个站点的文案）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// sessionDeadMarkers 命中即判定凭证失效（需重新登录，非 refresh 可修）。
var sessionDeadMarkers = []string{
	"invalid token", "token invalid", "user not login", "not logged in",
	"login expired", "refresh token expired", "invalid refresh token",
	"unauthorized", "登录已失效", "凭证失效", "请重新登录",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	if strings.Contains(body, "10605") || strings.Contains(lower, "busy") {
		return ErrBusy
	}
	// TOKEN_EXPIRE 优先于通用 401：前者 refresh 可修，后者不可用。
	if status == 401 && strings.Contains(body, "TOKEN_EXPIRE") {
		return ErrTokenExpired
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	switch {
	case status == 402:
		return ErrHardCredit
	case status == 429:
		return ErrSoftRate
	case status == 404:
		return ErrNotFound
	case status == 401, status == 403:
		return ErrSessionDead
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// ClassifyError 判定并把结果包装成 *Error。
func ClassifyError(status int, body string) error {
	kind := Classify(status, body)
	if kind == ErrNone {
		return nil
	}
	return &Error{Kind: kind, Status: status, Msg: strings.TrimSpace(body)}
}

// IsKind 工具函数。
func IsKind(err error, kind ErrKind) bool {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Kind == kind
	}
	return false
}
