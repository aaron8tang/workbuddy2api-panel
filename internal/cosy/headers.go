package cosy

import (
	"net/http"
	"strings"
)

// HeaderParams COSY 请求头构造参数（每请求变体部分，静态部分见 Profile）。
type HeaderParams struct {
	Body            []byte // 上行 body（已用私有字母表编码），签名覆盖其原始字节
	RawURL          string // 完整 URL，签名只取 Path（去 /algo 前缀、去 query）
	UID             string // Cosy-User
	RequestID       string // payload.requestId；空则自动生成（Traceparent 亦派生自此）
	ModelKey        string // X-Model-Key；空表示不发送模型头（控制面请求）
	ModelSource     string // X-Model-Source（有 ModelKey 时才发，允许空串）
	Accept          string // 缺省 text/event-stream
	SSE             bool   // 是否发 Cache-Control: no-cache
	OrgID           string // Cosy-Organization-Id；空则不发该头
	OrgTags         []string
	MachineHostname string // Cosy-MachineHostname；空则不发（千问办公实测带主机名）
}

// ApplyHeaders 按 Profile 铺设全套 COSY 头并用当前会话签名。
//
// 两侧差异（客户端类型、场景、UA、login-version、clientip、traceparent…）
// 全部由 Profile 描述，故 QoderWork 与 Qoder 共用此实现：
// 空值头一律不发送——上游会拒绝携带空 organization/model 头的请求。
func (s *Session) ApplyHeaders(req *http.Request, p Profile, hp HeaderParams) error {
	reqID := hp.RequestID
	if reqID == "" {
		reqID = UUID()
	}
	auth, err := s.Authorization(hp.Body, SignedPath(hp.RawURL), Payload{
		Order:       p.PayloadOrder,
		Version:     "v1",
		CosyVersion: p.CosyVersion,
		IDEVersion:  "",
		RequestID:   reqID,
	}, nowFunc())
	if err != nil {
		return err
	}
	if req.Header == nil {
		req.Header = make(http.Header, 32)
	}
	set := func(key, value string) {
		if value == "" {
			return
		}
		req.Header.Set(key, value)
	}
	set("Accept", HeaderOrDefault(hp.Accept, "text/event-stream"))
	for _, kv := range p.ExtraHeaders {
		set(kv[0], kv[1])
	}
	if hp.SSE {
		set("Cache-Control", "no-cache")
	}
	set("Content-Type", "application/json")
	set("Cosy-Business-Product", p.BusinessProduct)
	set("Cosy-Business-Type", p.BusinessType)
	set("Cosy-ClientType", p.ClientType)
	set("Cosy-Data-Policy", p.DataPolicy)
	set("Cosy-Date", unixSeconds(nowFunc()))
	set("Cosy-Key", s.CosyKey)
	set("Cosy-MachineId", s.fp.MachineID)
	set("Cosy-MachineOS", p.MachineOS)
	set("Cosy-MachineToken", s.fp.MachineToken)
	set("Cosy-MachineType", p.MachineTypeOr(s.fp.MachineType))
	if orgID := strings.TrimSpace(hp.OrgID); orgID != "" {
		set("Cosy-Organization-Id", orgID)
	}
	if tags := filterTags(hp.OrgTags); len(tags) > 0 {
		set("Cosy-Organization-Tags", strings.Join(tags, ","))
	}
	set("Cosy-Scene", p.Scene)
	set("Cosy-User", strings.TrimSpace(hp.UID))
	set("Cosy-Version", p.CosyVersion)
	set("Cosy-MachineHostname", hp.MachineHostname)
	set("Cosy-Clientip", clientIPOf(p, s))
	set("Login-Version", p.LoginVersion)
	if p.TraceContext {
		set("Traceparent", Traceparent(reqID))
	}
	set("User-Agent", p.UserAgent)
	set("Accept-Encoding", p.AcceptEncoding)
	set("Authorization", auth)
	if key := strings.TrimSpace(hp.ModelKey); key != "" {
		set("X-Model-Key", key)
		// source 头随 key 门控而非自身取值空否——CLI 在 source 为空时仍发送该头。
		set("X-Model-Source", strings.TrimSpace(hp.ModelSource))
	}
	return nil
}

// clientIPOf 解析 Cosy-Clientip 取值："@machine" 占位符用账号机器 ID
//（千问办公实测 clientip=machineid），其余原样返回。
func clientIPOf(p Profile, s *Session) string {
	if p.ClientIP == "@machine" {
		return s.fp.MachineID
	}
	return p.ClientIP
}

// ApplyControlHeaders 铺设开放/业务 API 的 Bearer(dt) 头（无 COSY 签名）。
func ApplyControlHeaders(req *http.Request, p Profile, dt string, body bool) {
	if req.Header == nil {
		req.Header = make(http.Header, 8)
	}
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	if body {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", p.ControlUserAgent())
}

// filterTags 丢弃空白组织标签；结果空表示不发送 Cosy-Organization-Tags。
func filterTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if s := strings.TrimSpace(t); s != "" {
			out = append(out, s)
		}
	}
	return out
}
