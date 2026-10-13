// logging.go trae 渠道请求级表格日志：每个 chat 请求结束后打印一行到 stdout。
// 移植自 trae2api（原 internal/server/logging.go），模型列带 "trae:" 前缀区分渠道。
package trae

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时取「昵称(uid8)」标签
	nick   string // 账号昵称（随选号同步，可与 uid 一起上报外层）
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// onAccount 选号上报回调（server 分发时注入）：trae 子系统不写 reqlog，
	// 面板「请求记录」的账号列由 server 侧 chatStat 填充，经此回调免 import cycle。
	onAccount func(uid, nick string)
	logged    bool
}

// setAccount 记录本次请求实际使用的账号，并上报外层统计（server 侧 chatStat →
// reqlog.Event.Account → 面板请求记录账号列）。每次轮转/Work 兜底选到号即调用，
// 最终生效的是最后一次（即实际服务请求的账号）。
func (s *chatStat) setAccount(uid, nick string) {
	s.uid, s.nick = uid, nick
	if s.onAccount != nil {
		s.onAccount(uid, nick)
	}
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks)
}

// soloStatsReader 解析原生 SOLO SSE（event:/data: 双行），记录首个 output 事件的
// TTFB 与 token_usage 事件的 completion_tokens / total_tokens。原始字节原样返回
// 给下游透传。total_tokens 供积分预扣（pool.DeductCredits 按倍率×tokens 估算），
// completion_tokens 仅供日志 tok 列。
type soloStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool   // 已见过首个 output 事件（TTFB 只记一次）
	hasUsage  bool   // 末帧是否带 token_usage
	tokens    int    // completion_tokens（日志列）
	hasTotal  bool   // 是否捕获到 total_tokens
	totalToks int    // total_tokens（积分预扣数据源）
	pend      []byte // 已读未返回的行缓存
	event     string
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *soloStatsReader {
	return &soloStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 output 事件到达耗时；无帧时为 0。
func (s *soloStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回 token_usage 的 completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *soloStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// UsageTokens 返回 token_usage 的 total_tokens 与是否缺失；缺失时 ok=false。
// 积分按总 tokens（prompt+completion+reasoning 等）计费，completion 口径会系统性
// 低估消耗，故预扣走 total、日志列维持 completion 不变。
func (s *soloStatsReader) UsageTokens() (int, bool) { return s.totalToks, s.hasTotal }

// parseSSELine 解析原生 SOLO 事件行：首个 output 事件记 TTFB，token_usage 事件采信 usage。
func (s *soloStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "event:"):
		s.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return
	case strings.HasPrefix(line, "data:"):
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			return
		}
		switch s.event {
		case "output":
			if !s.seen {
				s.seen = true
				s.ttfb = time.Since(s.start)
			}
		case "token_usage":
			var u struct {
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}
			if json.Unmarshal([]byte(payload), &u) == nil && u.CompletionTokens > 0 {
				s.hasUsage = true
				s.tokens = u.CompletionTokens
				if u.TotalTokens > 0 {
					s.hasTotal = true
					s.totalToks = u.TotalTokens
				}
			}
		}
		return
	default:
		// OpenAI 兼容透传行（如 data: {...}）的兜底：首帧记 TTFB，末帧 usage 采信。
		if !strings.HasPrefix(line, "data:") {
			return
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return
		}
		if !s.seen {
			s.seen = true
			s.ttfb = time.Since(s.start)
		}
		var chunk struct {
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
			s.hasUsage = true
			s.tokens = chunk.Usage.CompletionTokens
			if chunk.Usage.TotalTokens > 0 {
				s.hasTotal = true
				s.totalToks = chunk.Usage.TotalTokens
			}
		}
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *soloStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// soloKnownFields 诊断白名单：与 upstream.soloAllowedFields 同口径（SOLO 协议
// 字段 + OpenAI 常规采样参数 + 服务端注入的 reasoning_effort）。此外的字段
// 视为调用方私有字段，logUnexpectedFields 会打日志观测。
var soloKnownFields = map[string]bool{
	"messages": true, "stream": true, "function": true, "config_name": true,
	"model": true, "tools": true, "tool_choice": true,
	"temperature": true, "top_p": true, "max_tokens": true, "top_k": true,
	"frequency_penalty": true, "presence_penalty": true,
	"stop": true, "seed": true, "reasoning_effort": true,
	"reasoning": true, "thinking": true,
}

// logRequestBodyProfile 打印一行请求体画像（定位上游 3004 类拒绝由何种请求
// 特征触发：超大 body / 超长多轮对话 / 含工具调用历史）。在 logUnexpectedFields
// 之前调用，与失败请求的容器日志时间线对齐。
func logRequestBodyProfile(body []byte) {
	var obj struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		log.Printf("trae_chat: body profile bytes=%d parse=failed", len(body))
		return
	}
	toolCallHistory, toolMsgs := 0, 0
	for _, raw := range obj.Messages {
		if bytes.Contains(raw, []byte(`"tool_calls"`)) {
			toolCallHistory++
		}
		if bytes.Contains(raw, []byte(`"tool_call_id"`)) {
			toolMsgs++
		}
	}
	log.Printf("trae_chat: body profile bytes=%d messages=%d tools=%d assistant_with_tool_calls=%d tool_results=%d",
		len(body), len(obj.Messages), len(obj.Tools), toolCallHistory, toolMsgs)
}

// logUnexpectedFields 扫描入站 body 顶层字段：SOLO 已知集合之外的字段逐个打
// 日志（值截断）。用于定位 agent CLI / 公网调用方在 body 里塞的私有字段被
// 原样透传给上游触发的 400/4028 拒绝（如 destination-addr 类网关校验）。
func logUnexpectedFields(body []byte) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return
	}
	for k, raw := range obj {
		if soloKnownFields[k] {
			continue
		}
		v := string(raw)
		if len(v) > 120 {
			v = v[:120] + "..."
		}
		log.Printf("trae_chat: unexpected body field %q = %s", k, v)
	}
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageTotalTokens 从非流式响应提取 usage.total_tokens；缺失返回 -1。
// 流式路径对应 soloStatsReader.UsageTokens，两者同为积分预扣数据源。
func usageTotalTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["total_tokens"].(float64)
	if !ok || v <= 0 {
		return -1
	}
	return int(v)
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// 账号列与 server 侧流水行同口径：「昵称(uid8)」（logfmt.Label），昵称缺省退 uid8。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	seq := chatSeq.Add(1)
	if len(model) > 20 {
		model = model[:20]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| trae #%03d | %s | %s | %s | %d | acct=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		logfmt.Label(uid, nick),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
