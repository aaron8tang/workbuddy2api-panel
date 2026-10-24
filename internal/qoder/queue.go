// queue.go Qoder 模型排队协议（复刻 Qoder CN 桌面端 qoder-worker-runtime.obf.mjs
// 的 [ModelQueue] 实现）：
//   - 流内 10605 错误帧（isQueued:true）≠ 错误，是「请求已进入模型队列」；
//     服务端为请求体里的 request_set_id 建 lease
//   - 轮询 GET /algo/api/v2/service/ask/queue/status（auth 签名 GET +
//     X-Request-ID/X-Session-ID 头）直到 isQueued:false / serviceAvailable:true
//   - 出队后等 retryAfterSeconds，以同一 request_set_id 重发聊天请求
//     （request_id/chat_record_id 刷新）；重发上限 queueMaxResend
//   - 请求结束后 POST /algo/api/v2/service/ask/finish 上报耗时（尽力而为，
//     失败由服务端 TTL 兜底释放 lease）
package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

const (
	queueStatusPath = "/algo/api/v2/service/ask/queue/status"
	queueFinishPath = "/algo/api/v2/service/ask/finish"
	// queueMaxWait 单次请求的排队等待总预算（桌面端按 requestTimeoutMs，取 10 分钟）。
	queueMaxWait = 10 * time.Minute
	// queueMaxResend 出队后重发聊天请求的上限（正常一次即成，防御 lease 反复拒答）。
	queueMaxResend = 3
	// queuePollFailures 轮询连续失败上限（超即放弃，交由网关按 Busy 处置换号）。
	queuePollFailures = 3
)

// queueInfo queue/status 响应（字段对齐桌面端 rsl 解析）。
type queueInfo struct {
	IsQueued          *bool
	ServiceAvailable  *bool
	ModelKey          string
	QueueCount        *int64
	QueueType         string
	RetryAfterSeconds *int64
	WaitTime          *int64
}

// wait 返回建议等待秒数：优先 retryAfterSeconds，其次 waitTime；无信号返回 0。
func (q queueInfo) wait() int64 {
	if q.RetryAfterSeconds != nil && *q.RetryAfterSeconds > 0 {
		return *q.RetryAfterSeconds
	}
	if q.WaitTime != nil && *q.WaitTime > 0 {
		return *q.WaitTime
	}
	return 0
}

// drained 是否已出队（桌面端判定：isQueued === false 或 serviceAvailable === true）。
func (q queueInfo) drained() bool {
	if q.ServiceAvailable != nil && *q.ServiceAvailable {
		return true
	}
	if q.IsQueued != nil && !*q.IsQueued {
		return true
	}
	return false
}

// queuePeek 首帧 peek 结果：queued 非 nil = 首帧是 10605 排队帧。
// br 持有 bufio 缓冲（预读的多帧数据在里面），rejoin 必须带上。
type queuePeek struct {
	head   []byte // 已读的原始字节（回灌流头）
	br     *bufio.Reader
	queued *cosy.InStreamError
}

// rejoin 把已读字节与剩余流（含 bufio 缓冲）重新拼接为一个可读可关的流。
func (p *queuePeek) rejoin(rc io.ReadCloser) io.ReadCloser {
	return rejoinReader{Reader: io.MultiReader(bytes.NewReader(p.head), p.br), closer: rc}
}

type rejoinReader struct {
	io.Reader
	closer io.Closer
}

func (r rejoinReader) Close() error { return r.closer.Close() }

// isModelQueued 判定错误帧是否为 10605 排队帧（与 provider_error 等其他 Busy 类
// 区分——只有 10605 才有 queue/status 可查，其余按 Busy 短冷却换号）。
func isModelQueued(ie *cosy.InStreamError) bool {
	if ie.Code == "10605" || strings.Contains(ie.Msg, "10605") {
		return true
	}
	// message 内嵌 JSON 剥壳：{"code":10605,"isQueued":true,...,"subCode":"RESOURCE_LIMIT"}
	var inner struct {
		Code    any    `json:"code"`
		SubCode string `json:"subCode"`
	}
	if err := json.Unmarshal([]byte(ie.Msg), &inner); err == nil {
		if fmt.Sprint(inner.Code) == "10605" || inner.SubCode == "RESOURCE_LIMIT" {
			return true
		}
	}
	return false
}

// peekQueueFrame 读流首帧（读至第一个 data: 行），判定是否 10605 排队帧。
// 非 10605 时已读字节保留在 peek.head，调用方经 rejoin 还回流头。
func peekQueueFrame(rc io.ReadCloser) (*queuePeek, error) {
	br := bufio.NewReaderSize(rc, 64*1024)
	var head []byte
	for {
		line, err := br.ReadString('\n')
		head = append(head, line...)
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data: "))
			if payload != "" && payload != "[DONE]" {
				var env cosy.Envelope
				if json.Unmarshal([]byte(payload), &env) == nil {
					body := strings.TrimSpace(env.Body)
					if body != "" && body != "[DONE]" {
						var chunk map[string]any
						if json.Unmarshal([]byte(body), &chunk) == nil {
							if ie, ok := cosy.ErrFrameOf(chunk); ok && isModelQueued(ie) {
								return &queuePeek{head: head, br: br, queued: ie}, nil
							}
						}
					}
				}
			}
			return &queuePeek{head: head, br: br}, nil
		}
		if err != nil {
			if err == io.EOF {
				return &queuePeek{head: head, br: br}, nil
			}
			return nil, err
		}
	}
}

// logLabel 账号标签（昵称(uid8)），与 logfmt.Label 同形。
func logLabel(uid, nick string) string {
	if len(uid) > 8 {
		uid = uid[:8]
	}
	if nick == "" {
		return uid
	}
	return nick + "(" + uid + ")"
}

// waitForQueue 排队轮询循环：收到 10605 后调用，阻塞直到出队/超时/ctx 取消。
// 返回 nil = 可以重发聊天请求。
func (c *Client) waitForQueue(ctx context.Context, acct *cosy.Account, cr *Cred, opts ChatOptions, first *cosy.InStreamError, deadline time.Time) error {
	q := parseQueueInfoFromError(first)
	uid := logLabel(acct.UID, acct.Nickname)
	log.Printf("[qoder] uid=%s model=%s: model queue entered (request_set_id=%s queue_type=%s retry_after=%ds)",
		uid, opts.Model.Key, opts.RequestSetID, q.QueueType, q.wait())

	fails := 0
	lastCount, lastWait := int64(-1), int64(-1)
	for {
		if q.drained() {
			// 出队：按帧内 retryAfterSeconds 再等一小段（桌面端同款），随后重发
			if d := q.wait(); d > 0 {
				if err := sleepCtx(ctx, minDur(time.Duration(d)*time.Second, time.Until(deadline))); err != nil {
					return err
				}
			}
			log.Printf("[qoder] uid=%s request_set_id=%s: queue drained, resending", uid, opts.RequestSetID)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return &cosy.Error{Kind: cosy.ErrBusy, Msg: "model queue wait timeout (request_set_id=" + opts.RequestSetID + ")"}
		}
		// 轮询间隔：帧内 retryAfterSeconds 优先，缺省 10s，钳 [3s, 30s]
		waitSec := q.wait()
		if waitSec <= 0 {
			waitSec = 10
		}
		wait := minDur(maxDur(time.Duration(waitSec)*time.Second, 3*time.Second), 30*time.Second)
		if remain := time.Until(deadline); wait > remain {
			wait = remain
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
		next, err := c.queueStatus(ctx, acct, cr, opts)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			fails++
			if fails >= queuePollFailures {
				return &cosy.Error{Kind: cosy.ErrBusy, Msg: fmt.Sprintf("queue status poll failed %d times: %v", fails, err)}
			}
			log.Printf("[qoder] uid=%s request_set_id=%s: queue poll failed %d/%d: %v", uid, opts.RequestSetID, fails, queuePollFailures, err)
			continue
		}
		fails = 0
		// 状态变化（队列人数/等待时长）时节流打一条，便于观测排队进度
		if (next.QueueCount != nil && *next.QueueCount != lastCount) || (next.WaitTime != nil && *next.WaitTime != lastWait) {
			if next.QueueCount != nil {
				lastCount = *next.QueueCount
			}
			if next.WaitTime != nil {
				lastWait = *next.WaitTime
			}
			log.Printf("[qoder] uid=%s request_set_id=%s: queue waiting (queue_count=%v wait_time=%vs is_queued=%v service_available=%v)",
				uid, opts.RequestSetID, next.QueueCount, next.WaitTime, next.IsQueued, next.ServiceAvailable)
		}
		q = next
	}
}

// queueStatus 查询排队状态（auth 签名 GET + X-Request-ID/X-Session-ID 头，
// 形态对齐桌面端 bbt.getStatus）。
func (c *Client) queueStatus(ctx context.Context, acct *cosy.Account, cr *Cred, opts ChatOptions) (queueInfo, error) {
	qv := url.Values{"requestSetId": {opts.RequestSetID}, "modelKey": {opts.Model.Key}, "queueType": {queueTypeOf(opts)}}
	rawURL := c.Base + queueStatusPath + "?" + qv.Encode()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return queueInfo{}, err
	}
	key, info, err := cr.EnsureRuntime()
	if err != nil {
		return queueInfo{}, err
	}
	sess, err := cosy.NewSessionFromRuntime(acct.Fingerprint, cr.identity(), key, info)
	if err != nil {
		return queueInfo{}, err
	}
	if err := sess.ApplyHeaders(req, cosy.ProfileQoder(), cosy.HeaderParams{
		RawURL:    rawURL,
		UID:       acct.UID,
		OrgID:     cr.OrgID,
		OrgTags:   cr.OrgTags,
		RequestID: opts.RequestID,
		Accept:    "application/json",
	}); err != nil {
		return queueInfo{}, err
	}
	req.Header.Set("X-Request-ID", opts.RequestID)
	req.Header.Set("X-Session-ID", opts.SessionID)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return queueInfo{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode != http.StatusOK {
		return queueInfo{}, fmt.Errorf("queue status http %d: %.200s", resp.StatusCode, raw)
	}
	return parseQueueInfo(raw), nil
}

// FinishRequest 上报排队结束与耗时（fire-and-forget；失败仅日志，服务端 TTL
// 兜底释放 lease）。成功消费一跳后由网关调用。
func (c *Client) FinishRequest(ctx context.Context, acct *cosy.Account, cr *Cred, opts ChatOptions, timeConsumedMs int64) error {
	payload, _ := json.Marshal(map[string]any{
		"model_key":      opts.Model.Key,
		"request_set_id": opts.RequestSetID,
		"user_id":        "",
		"time_consumed":  max64(0, timeConsumedMs/1e6),
	})
	body, _ := json.Marshal(map[string]any{"payload": string(payload), "encodeVersion": "1"})

	rawURL := c.Base + queueFinishPath + "?Encode=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	key, info, err := cr.EnsureRuntime()
	if err != nil {
		return err
	}
	sess, err := cosy.NewSessionFromRuntime(acct.Fingerprint, cr.identity(), key, info)
	if err != nil {
		return err
	}
	if err := sess.ApplyHeaders(req, cosy.ProfileQoder(), cosy.HeaderParams{
		Body:      body,
		RawURL:    rawURL,
		UID:       acct.UID,
		OrgID:     cr.OrgID,
		OrgTags:   cr.OrgTags,
		RequestID: opts.RequestSetID, // 桌面端 requestId(A.requestSetId)
		Accept:    "application/json",
	}); err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("queue finish http %d: %.200s", resp.StatusCode, raw)
	}
	return nil
}

// parseQueueInfoFromError 从 10605 错误帧的 message（内嵌 JSON）提取初始排队状态。
func parseQueueInfoFromError(ie *cosy.InStreamError) queueInfo {
	q := parseQueueInfo([]byte(ie.Msg))
	if q.QueueType == "" && !strings.Contains(ie.Msg, "{") {
		// message 非内嵌 JSON 时从原始帧文本抓 queueType
		q.QueueType = extractJSONString(ie.Msg, "queueType")
	}
	return q
}

// parseQueueInfo 解析 queue/status 响应：剥 data/result/message/body 包层
//（字符串值再 JSON 解析一层，对齐桌面端 nsl 递归）后取排队字段。
func parseQueueInfo(raw []byte) queueInfo {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return queueInfo{}
	}
	for depth := 0; depth < 4; depth++ {
		obj, ok := v.(map[string]any)
		if !ok {
			break
		}
		if q, ok := queueInfoOf(obj); ok {
			return q
		}
		next, has := peel(obj, "data", "result", "body", "message")
		if !has {
			break
		}
		v = next
	}
	return queueInfo{}
}

// queueInfoOf 从对象提取排队字段；无任何排队键返回 false。
func queueInfoOf(obj map[string]any) (queueInfo, bool) {
	var q queueInfo
	found := false
	if v, ok := boolOf(obj["isQueued"]); ok {
		q.IsQueued = &v
		found = true
	}
	if v, ok := boolOf(obj["serviceAvailable"]); ok {
		q.ServiceAvailable = &v
		found = true
	}
	if v, ok := num64Of(obj["queueCount"]); ok {
		q.QueueCount = &v
	}
	if v, ok := num64Of(obj["retryAfterSeconds"]); ok {
		q.RetryAfterSeconds = &v
	}
	if v, ok := num64Of(obj["waitTime"]); ok {
		q.WaitTime = &v
	}
	if s, ok := obj["modelKey"].(string); ok {
		q.ModelKey = s
	}
	if s, ok := obj["queueType"].(string); ok {
		q.QueueType = s
	}
	return q, found
}

// peel 按 keys 顺序找第一个可下钻字段（字符串值尝试 JSON 解析）。
func peel(obj map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				return parsed, true
			}
			continue
		}
		if _, isMap := v.(map[string]any); isMap {
			return v, true
		}
	}
	return nil, false
}

func boolOf(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func num64Of(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// extractJSONString 从可能含 JSON 的文本里抓 key 的字符串值（尽力而为）。
func extractJSONString(s, key string) string {
	if i := strings.Index(s, `"`+key+`"`); i >= 0 {
		rest := s[i+len(key)+1:]
		rest = strings.TrimLeft(rest, " \t:")
		if j := strings.Index(rest, `"`); j >= 0 {
			rest = rest[j+1:]
			if k := strings.Index(rest, `"`); k >= 0 {
				return rest[:k]
			}
		}
	}
	return ""
}

// queueTypeOf 轮询 query 的 queueType：实测帧唯一队列型 "p4"。
// （桌面端仅在帧带 queueType 时附加；我们的 10605 帧恒带 p4。）
func queueTypeOf(opts ChatOptions) string { return "p4" }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
