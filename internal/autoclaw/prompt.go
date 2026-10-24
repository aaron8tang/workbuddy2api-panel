// prompt.go system 提示闸门（2026-09-22 起两地区同闸；agent2api prompt.rs 移植）：
// 必须以固定身份前缀开头且带 "## Tooling" 段，且不得含外来 harness 身份句。
// 不满足 → 上游 406 空响应体（完全不满足）或 403 pay-view（只缺 Tooling 段）。
package autoclaw

import "strings"

const (
	// IdentityLine 身份行（幂等判据：首条 system 以它开头即视为已处理）。
	IdentityLine = "You are a personal assistant running inside OpenClaw."
	// ToolingHeader Tooling 段标题（必须出现）。
	ToolingHeader = "## Tooling"
	// promptPrefix 注入用完整前缀（长前缀原文照抄）。
	promptPrefix = IdentityLine + "\n\n## Tooling\nAvailable tools are policy-filtered. Names are case-sensitive; call exactly as listed.\n"
)

// rewrites 身份句改写表（长在前——短条目是长条目的前缀，先长后短才不互相吃掉）；
// 大小写不敏感字面匹配（strings.Contains 的 EqualFold 版）。
var rewrites = []struct{ from, to string }{
	{"You are ZCode, an interactive coding agent", "You are an interactive coding agent"},
	{"You are a coding agent running in the Codex CLI tool", "You are a coding agent running in a terminal CLI tool"},
	{"You are running as a coding agent in the Codex CLI", "You are running as a coding agent in a terminal CLI"},
	{"You are a coding agent running in the Codex CLI", "You are a coding agent running in a terminal CLI"},
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent powered by a local coding harness"},
	{"You are Claude Code", "You are a coding assistant"},
	{"You are ZCode", "You are an interactive coding agent"},
	{"You are Codex", "You are a coding agent"},
}

// 外来 harness 身份句黑名单（大小写不敏感；出现在 system/developer 消息即命中）。
var harnessPhrases = []string{
	"You are ZCode",
	"You are Claude Code",
	"You are Codex",
	"Codex CLI",
	"DeepSeek Harness",
}

// containsFold 大小写不敏感子串。
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// messagesBody 请求体 messages 形态（松散解析：role/content 都可能是 any）。
type messagesBody struct {
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

// PromptNeedsGate 判请求体是否需要过 system 闸门（返回 needs, err）。
// 判据（对齐 prompt.rs）：首条 system/developer 消息 content 以 IdentityLine 开头
// 且全文含 ToolingHeader ⇒ 已处理；否则需要闸门。content 是字符串或
// [{type:text,text:...}] 数组两种形态都认。
func PromptNeedsGate(body map[string]any) (bool, error) {
	var mb messagesBody
	if err := decodeLoose(body, &mb); err != nil {
		return false, err
	}
	for _, m := range mb.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		text := contentText(m.Content)
		if strings.HasPrefix(text, IdentityLine) && strings.Contains(text, ToolingHeader) {
			return false, nil
		}
		// 第一条 system 消息即判定点（不扫全部——对齐源实现「首条消息」语义）。
		return true, nil
	}
	return true, nil // 没有 system 消息：插入纯前缀
}

// ApplyPromptGate 就地改写 body（PromptNeedsGate 为 true 时调用）：
// 1) 首 system 的外来身份句改写为中性表述；2) 不满足前缀+Tooling ⇒ 在
// messages[0] 插入纯前缀 system（首条不是 system 也不重排，对齐源实现）。
func ApplyPromptGate(body map[string]any) {
	raw, ok := body["messages"].([]any)
	if !ok || len(raw) == 0 {
		body["messages"] = []any{map[string]any{"role": "system", "content": promptPrefix}}
		return
	}
	// 改写首条 system/developer 里的外来身份句。
	for _, mv := range raw {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "system" && role != "developer" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			m["content"] = rewriteIdentities(c)
		case []any:
			for _, pv := range c {
				if p, ok := pv.(map[string]any); ok {
					if t, _ := p["type"].(string); t == "text" {
						if s, ok := p["text"].(string); ok {
							p["text"] = rewriteIdentities(s)
						}
					}
				}
			}
		}
		break
	}
	// 幂等判定（改写后再判一次）：仍不满足 ⇒ 插入前缀。
	if needs, _ := PromptNeedsGate(body); needs {
		inserted := make([]any, 0, len(raw)+1)
		inserted = append(inserted, map[string]any{"role": "system", "content": promptPrefix})
		body["messages"] = append(inserted, raw...)
	}
}

// rewriteIdentities 大小写不敏感替换（Go strings.Replace 不区分大小写 →
// 手写不区分大小写替换，保持其余部分原样）。
func rewriteIdentities(s string) string {
	for _, r := range rewrites {
		s = replaceFold(s, r.from, r.to)
	}
	return s
}

// replaceFold 大小写不敏感的全部替换。
func replaceFold(s, from, to string) string {
	ls, lf := strings.ToLower(s), strings.ToLower(from)
	if !strings.Contains(ls, lf) {
		return s
	}
	var sb strings.Builder
	for {
		i := strings.Index(ls, lf)
		if i < 0 {
			sb.WriteString(s)
			break
		}
		sb.WriteString(s[:i])
		sb.WriteString(to)
		s, ls = s[i+len(from):], ls[i+len(lf):]
	}
	return sb.String()
}

// HasHarnessIdentity 是否含外来 harness 身份句（诊断日志用；user 消息不算）。
func HasHarnessIdentity(body map[string]any) bool {
	var mb messagesBody
	if decodeLoose(body, &mb) != nil {
		return false
	}
	for _, m := range mb.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		text := contentText(m.Content)
		for _, p := range harnessPhrases {
			if containsFold(text, p) {
				return true
			}
		}
	}
	return false
}

// contentText content 的两种形态 → 纯文本。
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, pv := range c {
			if p, ok := pv.(map[string]any); ok {
				if t, _ := p["type"].(string); t == "text" {
					if s, ok := p["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	}
	return ""
}

// decodeLoose 松散 decode（any→struct）。
func decodeLoose(from, to any) error {
	b, err := jsonMarshal(from)
	if err != nil {
		return err
	}
	return jsonUnmarshal(b, to)
}
