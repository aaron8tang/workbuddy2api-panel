// Package modelgroup 模型组：把若干模型聚成一组，按组暴露独立的 OpenAI 兼容
// 路由（/{组名}/v1/chat/completions 与 /{组名}/v1/models，另有 /v1/{组名}/...
// 别名形态）。每个组可配置：
//
//   - Models   组内模型白名单。**使用顺序按当前生效积分倍率升序**（限时免费 = 0
//     排最前），倍率相同的保持书写顺序；客户端 model 缺省 / 等于组名时按此顺序取
//     第一个未被模型级限流的模型。空 = 不限（组内可用全部模型）。
//   - Accounts 组内账号优先级（按序逐个尝试，跳过冷却/占满/禁用号）；空 = 完全
//     跟随账号池既有选号规则（成本分层、快过期加权、粘性会话）。
//
// 配置来源：config.json 的 model_groups.groups 段（面板「模型与档位 → 模型组」
// 可在线编辑）；Registry 用原子指针持有快照，面板保存后整体替换、立即生效（无
// 需重启——路由 pattern 注册的是通配 {group}，组是否存在在请求期查表判定）。
package modelgroup

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
)

// Group 单个模型组。
type Group struct {
	// Name 组名，同时是 URL 路径段（/dev/v1/... 中的 "dev"）。
	Name string `json:"name"`
	// Models 组内模型（可为 "cn:glm-5.2" / "global:..." 带域前缀形态，判等按
	// 裸名）。使用顺序 = 生效积分倍率升序（免费在前），倍率相同则按本清单顺序；
	// 空 = 组内不限模型。
	Models []string `json:"models,omitempty"`
	// Accounts 组内账号 UID 优先级（按序尝试）。空 = 跟随账号池既有规则选号。
	Accounts []string `json:"accounts,omitempty"`
}

// nameRe 组名规则：小写字母/数字开头，允许小写字母、数字、连字符、下划线，
// 总长 1-32。URL 路径段一律小写，避免大小写歧义；面板编辑器同规则前置校验。
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// reserved 组名保留字：与网关既有顶层路径段冲突，禁止用作组名。
var reserved = map[string]bool{
	"v1": true, "panel": true, "status": true, "healthz": true,
	"static": true, "assets": true, "favicon.ico": true,
}

// ValidName 报告组名是否合法且未与既有路径冲突。
func ValidName(name string) bool { return nameRe.MatchString(name) && !reserved[name] }

// Validate 校验组清单：组名合法（ValidName）、不重复；模型/账号条目去空白、
// 去空行。原地规范化（trim 后写回）。任一错误即整体拒绝（fail fast，与
// config.normalize 的既有风格一致）。
func Validate(groups []Group) error {
	seen := map[string]bool{}
	for i := range groups {
		g := &groups[i]
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			return fmt.Errorf("model_groups.groups[%d]: 组名为空", i)
		}
		if !ValidName(g.Name) {
			return fmt.Errorf("model_groups.groups[%d]: 组名 %q 不合法"+
				"（须为小写字母/数字开头，仅含小写字母、数字、-、_，长度 1-32，且不与 v1/panel/status/healthz 等保留路径冲突）",
				i, g.Name)
		}
		if seen[g.Name] {
			return fmt.Errorf("model_groups.groups[%d]: 组名 %q 重复", i, g.Name)
		}
		seen[g.Name] = true
		g.Models = trimList(g.Models)
		g.Accounts = trimList(g.Accounts)
	}
	return nil
}

// trimList 去除每项首尾空白并丢弃空项（就地返回新切片，不改写入参底层数组语义）。
func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// BareOf 与 server.resolveModel 同一协议的镜像实现（server 包已 import 本包，
// 不能反向依赖）：剥 "cn:" / "global:" 域前缀取裸模型名；其余形态原样返回。
func BareOf(model string) string {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return model
	}
	return model[idx+1:]
}

// Allows 报告 bare（裸模型名）是否属于组内模型白名单。白名单为空 = 不限。
func (g *Group) Allows(bare string) bool {
	if len(g.Models) == 0 {
		return true
	}
	for _, m := range g.Models {
		if BareOf(m) == bare {
			return true
		}
	}
	return false
}

// HasAccount 报告 uid 是否在组内账号清单中。清单为空 = 未钉账号（false），
// 调用方（handler）只在清单非空时使用该判定。
func (g *Group) HasAccount(uid string) bool {
	for _, a := range g.Accounts {
		if a == uid {
			return true
		}
	}
	return false
}

// Registry 原子持有当前生效的组清单。读多写少（写仅发生在启动装配与面板
// 保存配置），不可变快照 + atomic 指针，读方无锁。
type Registry struct {
	p atomic.Pointer[[]Group]
}

// NewRegistry 以初始清单构建（ nil 清单 = 空注册表）。
func NewRegistry(groups []Group) *Registry {
	r := &Registry{}
	r.Replace(groups)
	return r
}

// Replace 整体替换组清单（调用方需已完成 Validate）。
func (r *Registry) Replace(groups []Group) {
	cp := make([]Group, len(groups))
	copy(cp, groups)
	r.p.Store(&cp)
}

// Lookup 按组名查组（精确匹配，大小写敏感——组名规则本身强制小写）。
func (r *Registry) Lookup(name string) (Group, bool) {
	if r == nil || !ValidName(name) {
		return Group{}, false
	}
	for _, g := range r.List() {
		if g.Name == name {
			return g, true
		}
	}
	return Group{}, false
}

// List 返回当前组清单快照（nil 注册表返回空切片，调用方无需判空）。
func (r *Registry) List() []Group {
	if r == nil {
		return nil
	}
	if s := r.p.Load(); s != nil {
		return *s
	}
	return nil
}
