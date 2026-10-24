// models.go 双模型标识路由（agent2api models.rs 移植）：
// X-Request-Model = 完整路由 ID（带通道前缀，决定计费通道）；
// body.model = 剥前缀后的模型 ID（上游认这个）。
// 剥前缀顺序先 zaicoding_ 后 zai_（反了会把 zaicoding_glm-5.3 错剥成 coding_glm-5.3）。
package autoclaw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// RoutePrefixZaiCoding Coding Plan 通道前缀。
	RoutePrefixZaiCoding = "zaicoding_"
	// RoutePrefixZai 会员/按量通道前缀。
	RoutePrefixZai = "zai_"
	// DefaultRoute 默认路由（服务端自动选型）。
	DefaultRoute = "zai_auto"
)

// ChatModel 一条模型（id 为客户端可见裸名；RouteID 为完整路由 ID）。
type ChatModel struct {
	ID            string `json:"id"`   // 裸名（剥前缀）
	RouteID       string `json:"routeId"`
	Name          string `json:"name,omitempty"`
	MaxInput      int64  `json:"max_input_tokens,omitempty"`
	MaxOutput     int64  `json:"max_output_tokens,omitempty"`
	SupportsImg   bool   `json:"supports_images,omitempty"`
	Reasoning     bool   `json:"supports_reasoning,omitempty"`
	Credits       string `json:"credits,omitempty"` // creditConsumptionLevel（低/中/高 档位文案）
	Source        string `json:"source,omitempty"`  // static | catalog
}

// staticFallback 静态兜底表（models.rs:165-184，对齐客户端当前可选项）。
var staticFallback = []ChatModel{
	{ID: "glm-5.3", RouteID: "zaicoding_glm-5.3", Name: "GLM-5.3",
		MaxInput: 1048576, MaxOutput: 307200, Reasoning: true, Source: "static"},
	{ID: "glm-5.3-flash", RouteID: "zai_glm-5.3-flash", Name: "GLM-5.3-Flash",
		MaxInput: 1048576, MaxOutput: 131072, Reasoning: true, SupportsImg: true, Source: "static"},
}

// catalogEntry 目录原始条目（id 就是完整路由 ID；也可能是裸字符串简写）。
type catalogEntry struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Input        []string `json:"input"`
	Reasoning    *bool    `json:"reasoning"`
	ContextWin   int64    `json:"contextWindow"`
	MaxTokens    int64    `json:"maxTokens"`
	CreditLevel  string   `json:"creditConsumptionLevel"`
}

// stripRoutePrefix 剥路由前缀得裸模型名（顺序：先 zaicoding_ 后 zai_）。
func stripRoutePrefix(routeID string) string {
	if rest, ok := strings.CutPrefix(routeID, RoutePrefixZaiCoding); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(routeID, RoutePrefixZai); ok {
		return rest
	}
	return routeID
}

// routeIDRe 合法路由 ID 形态：[a-z][a-z0-9]*_[A-Za-z0-9._:-]{1,127}。
var routeIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*_[A-Za-z0-9._:\-]{1,127}$`)

// catalog 缓存（进程级；失败保留上一份成功结果不清空——上游抖一下不至于
// 让模型列表消失）。
type catalogCache struct {
	mu      sync.Mutex
	entries []ChatModel
	fetched time.Time
}

// ---------------------------------------------------------------------------
// 路由解析（resolve_model_route 六档；models.rs:274-333）
// ---------------------------------------------------------------------------

// ResolveRoute 输入客户端请求的 model 名 → (路由 ID, 裸模型名)。
// 六档顺序：空→默认；静态表（id/name/routeId 大小写无关）→ 远程目录（同口径，
// 必须在兜底之前否则 zai_auto-fast 这类静默回退）→ 已知前缀 → 合法路由 ID 形态
// → 默认。
func (g *Gateway) ResolveRoute(model string) (routeID, bareModel string) {
	m := strings.TrimSpace(model)
	if m == "" {
		return DefaultRoute, stripRoutePrefix(DefaultRoute)
	}
	// 2. 静态表。
	for _, e := range staticFallback {
		if strings.EqualFold(e.ID, m) || strings.EqualFold(e.Name, m) || strings.EqualFold(e.RouteID, m) {
			return e.RouteID, e.ID
		}
	}
	// 3. 远程目录（在兜底默认之前——目录认识的必须按目录走）。
	for _, e := range g.catalogSnapshot() {
		if strings.EqualFold(e.ID, m) || strings.EqualFold(e.Name, m) || strings.EqualFold(e.RouteID, m) {
			return e.RouteID, stripRoutePrefix(e.RouteID)
		}
	}
	// 4. 已知前缀开头 → 原样当路由 ID（大小写原样保留）。
	if strings.HasPrefix(m, RoutePrefixZaiCoding) || strings.HasPrefix(m, RoutePrefixZai) {
		return m, stripRoutePrefix(m)
	}
	// 5. 合法路由 ID 形态 → 原样透传。
	if routeIDRe.MatchString(m) {
		return m, m
	}
	// 6. 兜底默认路由。
	return DefaultRoute, stripRoutePrefix(DefaultRoute)
}

// catalogSnapshot 当前缓存目录（空则同步拉一次）。
func (g *Gateway) catalogSnapshot() []ChatModel {
	g.cat.mu.Lock()
	if len(g.cat.entries) > 0 && time.Since(g.cat.fetched) < 5*time.Minute {
		out := g.cat.entries
		g.cat.mu.Unlock()
		return out
	}
	g.cat.mu.Unlock()
	if _, err := g.FetchCatalog(false); err != nil {
		return nil
	}
	g.cat.mu.Lock()
	defer g.cat.mu.Unlock()
	return g.cat.entries
}

// FetchCatalog 拉远程目录（catalog.rs）：GET {…}/autoclaw-proxy/proxy/autoclaw-model-config。
// 注意目录不在对话端点 /proxy/autoclaw 下一级而在上一级 /proxy/（lastIndexOf 语义）。
// 响应顶层直接 {"models":[...]}，没有 {code,data} 信封（同批兄弟接口有，不能照抄）。
// force=true 绕过 TTL；失败保留旧缓存。
func (g *Gateway) FetchCatalog(force bool) ([]ChatModel, error) {
	g.cat.mu.Lock()
	if !force && len(g.cat.entries) > 0 && time.Since(g.cat.fetched) < 5*time.Minute {
		out := g.cat.entries
		g.cat.mu.Unlock()
		return out, nil
	}
	cred := g.pickEnabled()
	g.cat.mu.Unlock()
	if cred == nil {
		return nil, fmt.Errorf("没有已启用的 %s 账号", g.region)
	}
	// 目录在 /proxy/ 上一级：{upstream 去掉尾 /autoclaw}/autoclaw-model-config。
	base := strings.TrimSuffix(Endpoints(g.region).Upstream, "/autoclaw")
	url := base + "/autoclaw-model-config"
	body, code, err := g.upstream.doJSON(http.MethodGet, url,
		signedAuthHeaders(cred.AccessToken), nil)
	if err != nil {
		return g.catSnapshotStale(), err
	}
	if code == http.StatusUnauthorized {
		return g.catSnapshotStale(), fmt.Errorf("模型目录返回 401（登录态失效，请刷新账号）")
	}
	if code != http.StatusOK {
		return g.catSnapshotStale(), fmt.Errorf("模型目录返回 HTTP %d", code)
	}
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return g.catSnapshotStale(), fmt.Errorf("模型目录不是合法 JSON: %w", err)
	}
	out := make([]ChatModel, 0, len(payload.Models))
	for _, rawEntry := range payload.Models {
		var s string
		if json.Unmarshal(rawEntry, &s) == nil && s != "" {
			// 裸字符串简写（如 "zai_auto"）。
			out = append(out, ChatModel{ID: stripRoutePrefix(s), RouteID: s, Source: "catalog"})
			continue
		}
		var e catalogEntry
		if json.Unmarshal(rawEntry, &e) != nil || e.ID == "" {
			continue
		}
		img := false
		for _, in := range e.Input {
			if in == "image" {
				img = true
				break
			}
		}
		reasoning := e.Reasoning == nil || *e.Reasoning // 缺省 true
		out = append(out, ChatModel{
			ID: stripRoutePrefix(e.ID), RouteID: e.ID,
			Name: e.Name, MaxInput: e.ContextWin, MaxOutput: e.MaxTokens,
			SupportsImg: img, Reasoning: reasoning, Credits: e.CreditLevel,
			Source: "catalog",
		})
	}
	if len(out) == 0 {
		return g.catSnapshotStale(), fmt.Errorf("模型目录没有返回任何模型（保留现有清单）")
	}
	g.cat.mu.Lock()
	g.cat.entries = out
	g.cat.fetched = time.Now()
	g.cat.mu.Unlock()
	return out, nil
}

// catSnapshotStale 失败时返回旧缓存（可能为空）。
func (g *Gateway) catSnapshotStale() []ChatModel {
	g.cat.mu.Lock()
	defer g.cat.mu.Unlock()
	return g.cat.entries
}

// ModelEntries /v1/models 与面板模型清单（id 带 "autoclaw:" 前缀）。
// 目录为空时回落静态兜底（零回归：无账号时本渠道整体不透出）。
func (g *Gateway) ModelEntries() []map[string]any {
	entries := g.catalogSnapshot()
	if len(entries) == 0 {
		entries = staticFallback
	}
	out := make([]map[string]any, 0, len(entries))
	for _, m := range entries {
		e := map[string]any{
			"id":       Prefix + ":" + m.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "autoclaw",
		}
		if m.Name != "" {
			e["display_name"] = m.Name
		}
		if m.MaxInput > 0 {
			e["context_length"] = m.MaxInput
		}
		if m.MaxOutput > 0 {
			e["max_output_tokens"] = m.MaxOutput
		}
		if m.SupportsImg {
			e["vision"] = true
		}
		if m.Credits != "" {
			e["credits"] = m.Credits
		}
		e["route_id"] = m.RouteID
		out = append(out, e)
	}
	return out
}

// Prefix 渠道模型前缀（resolve_model 枚举用）。
const Prefix = "autoclaw"
