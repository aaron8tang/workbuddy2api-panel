// login.go Web 登录闭环：生成登录 URL → pending 态 → /authorize 回调捕获 →
// ExchangeToken + GetUserInfo + 落盘 → 面板轮询 result。
//
// 回调端口策略（trae2api M0 结论：双端口 18080）：
//   - TRAE 登录页回调到 http://127.0.0.1:18080/authorize
//   - main 起第二个 http.Server 监听 18080（StartCallbackServer），
//     只服务 /authorize（AuthorzeCallback）；"0" 关闭
//   - 回调捕获后直接在服务端完成导入（用户无需再粘贴）
//
// pending 态只在内存（重启丢失），符合「登录态瞬时」语义。
package trae

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
)

// pendingState pending 登录状态。
type pendingState string

const (
	pendingStateActive   pendingState = "pending"  // 等待用户登录回调
	pendingStateSuccess  pendingState = "success"  // 回调捕获 + 落盘成功
	pendingStateFailed   pendingState = "failed"   // ExchangeToken/落盘失败
	pendingStateCanceled pendingState = "canceled" // 用户取消
)

// pendingLogin 单次登录的临时上下文。
type pendingLogin struct {
	state       pendingState
	machineID   string
	deviceID    string
	callbackURL string // auth_callback_url（含端口）
	createdAt   time.Time

	// success 时填
	uid      string
	nickname string
	// failed 时填
	errMsg string
}

// StartLogin 生成登录 URL + pending 态，返回 (loginURL, pendingID, callbackURL)。
// callbackPort 为回调监听端口（与 main 的 StartCallbackServer 一致）。
func (g *Gateway) StartLogin(callbackPort string) (loginURL, pendingID, callbackURL string, err error) {
	if callbackPort == "" {
		callbackPort = "18080"
	}
	callbackURL = "http://127.0.0.1:" + callbackPort + "/authorize"

	machineID, err := randomHex(16) // hex32
	if err != nil {
		return "", "", "", err
	}
	// deviceID 用「签到兼容」的 16 位纯数字（auth.NewCheckinDeviceID），
	// 与真实注册设备号格式一致，保证该账号随后每日签到成功且与其他账号互异。
	deviceID, err := auth.NewCheckinDeviceID()
	if err != nil {
		return "", "", "", err
	}
	loginURL = BuildLoginURL(machineID, deviceID, callbackURL)

	pendingID, err = randomHex(8) // hex16
	if err != nil {
		return "", "", "", err
	}
	g.loginMu.Lock()
	g.logins[pendingID] = &pendingLogin{
		state:       pendingStateActive,
		machineID:   machineID,
		deviceID:    deviceID,
		callbackURL: callbackURL,
		createdAt:   time.Now(),
	}
	g.loginMu.Unlock()
	return loginURL, pendingID, callbackURL, nil
}

// LoginResult 查询登录结果（面板轮询）。
func (g *Gateway) LoginResult(pendingID string) (map[string]any, bool) {
	pl, ok := g.getPending(pendingID)
	if !ok {
		return nil, false
	}
	resp := map[string]any{
		"pending_id": pendingID,
		"state":      string(pl.state),
	}
	if pl.state == pendingStateSuccess {
		resp["uid"] = pl.uid
		resp["nickname"] = pl.nickname
	} else if pl.state == pendingStateFailed {
		resp["error"] = pl.errMsg
	}
	return resp, true
}

// CancelLogin 取消登录（pending 删除）。
func (g *Gateway) CancelLogin(pendingID string) bool {
	g.loginMu.Lock()
	pl, ok := g.logins[pendingID]
	if ok && pl.state == pendingStateActive {
		pl.state = pendingStateCanceled
	}
	delete(g.logins, pendingID)
	g.loginMu.Unlock()
	return ok
}

// LoginStartHandler POST /panel/api/trae/login：生成登录 URL + pending 态。
// body 可选 {callback_port: "18080"}；缺省用 Gateway 配置的 callbackPort。
func (g *Gateway) LoginStartHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackPort string `json:"callback_port,omitempty"`
	}
	_ = decodeBodyOptional(r, &req)
	port := req.CallbackPort
	if port == "" {
		port = g.callbackPort
	}
	loginURL, pendingID, callbackURL, err := g.StartLogin(port)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"login_url": loginURL, "pending_id": pendingID, "callback_url": callbackURL,
	})
}

// LoginResultHandler GET /panel/api/trae/login/result?pending_id=...
func (g *Gateway) LoginResultHandler(w http.ResponseWriter, r *http.Request) {
	resp, ok := g.LoginResult(r.URL.Query().Get("pending_id"))
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "pending login not found (expired or invalid)")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// LoginCancelHandler POST /panel/api/trae/login/cancel（body {pending_id} 或 query ?pending_id=）
func (g *Gateway) LoginCancelHandler(w http.ResponseWriter, r *http.Request) {
	pendingID := r.URL.Query().Get("pending_id")
	if pendingID == "" {
		var req struct {
			PendingID string `json:"pending_id"`
		}
		_ = decodeBodyOptional(r, &req)
		pendingID = req.PendingID
	}
	ok := g.CancelLogin(pendingID)
	writeJSON(w, http.StatusOK, map[string]any{"pending_id": pendingID, "canceled": ok})
}

// AuthorizeCallback GET /authorize：TRAE 回调落点。
// 捕获 query → 解析 → ExchangeToken → GetUserInfo → 落盘 → 标记 pending 成功。
// pending_id 通过 TRAE 无法回传（回调 URL 固定），用 loginTraceID/machine_id 反查。
func (g *Gateway) AuthorizeCallback(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.String()
	if !strings.HasPrefix(rawURL, "http") {
		// /authorize?... → 补全成完整 URL 供 ParseCallback
		rawURL = "http://127.0.0.1" + rawURL
	}
	info, err := ParseCallback(rawURL)
	if err != nil {
		authorizeRender(w, http.StatusBadRequest, "登录回调解析失败", err.Error())
		return
	}
	// TRAE 回调不回传 machine_id/device_id，但回传 loginTraceID（= machineTraceID 派生）
	// → 用 loginTraceID 反查 pending 拿回登录时生成的那一对 id，保证凭证与登录态一致
	machineID := r.URL.Query().Get("machine_id")
	deviceID := r.URL.Query().Get("device_id")
	traceID := r.URL.Query().Get("loginTraceID")
	if machineID == "" || deviceID == "" {
		if traceID != "" {
			if pl, ok := g.getPendingByTrace(traceID); ok {
				machineID, deviceID = pl.machineID, pl.deviceID
			}
		}
	}

	a := &auth.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		UID:          info.UID,
		Nickname:     info.Nickname,
		EnterpriseID: info.EnterpriseID,
		Domain:       "trae.cn",
		ApiHost:      "https://api.trae.com.cn",
		MachineID:    machineID,
		DeviceID:     deviceID,
		ExpiresAt:    info.ExpiresAt,
	}
	// 有 refreshToken → ExchangeToken 换新 access + 轮换 refreshToken
	if a.RefreshToken != "" {
		if exErr := g.cfg.Upstream.RefreshToken(a); exErr != nil {
			// 失败也继续（可能 refreshToken 已被轮换），但标记错误
			g.markPendingByMachine(machineID, traceID, pendingStateFailed, "", "", exErr.Error())
			authorizeRender(w, http.StatusBadGateway, "ExchangeToken 失败", exErr.Error())
			return
		}
	}
	// GetUserInfo 补全 uid/nickname
	uid, nick, ent, gErr := g.cfg.Upstream.GetUserInfo(a)
	if gErr == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	if a.UID == "" {
		err := errors.New("cannot determine uid from callback or GetUserInfo")
		g.markPendingByMachine(machineID, traceID, pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	if a.AccessToken == "" {
		err := errors.New("no access token after exchange")
		g.markPendingByMachine(machineID, traceID, pendingStateFailed, "", "", err.Error())
		authorizeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}

	// 签到 deviceId 兜底（登录时已生成；粘贴回调导入等路径可能缺）
	_, _ = a.EnsureCheckinDeviceID()

	// 落盘
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(g.cfg.AuthDir, a.UID)
	}
	if mkErr := mkdirAll(g.cfg.AuthDir); mkErr != nil {
		g.markPendingByMachine(machineID, traceID, pendingStateFailed, a.UID, a.Nickname, mkErr.Error())
		authorizeRender(w, http.StatusInternalServerError, "落盘失败", mkErr.Error())
		return
	}
	if err := a.SaveAtomic(); err != nil {
		g.markPendingByMachine(machineID, traceID, pendingStateFailed, a.UID, a.Nickname, err.Error())
		authorizeRender(w, http.StatusInternalServerError, "落盘失败", err.Error())
		return
	}
	g.cfg.Pool.Add(a)
	g.markPendingByMachine(machineID, traceID, pendingStateSuccess, a.UID, a.Nickname, "")

	authorizeRender(w, http.StatusOK, "登录成功", "账号 "+a.UID+"（"+a.Nickname+"）已添加，可关闭此窗口返回面板。")
}

// markPendingByMachine 用 machineID（或 loginTraceID 派生匹配）反查 pending 并标记状态。
func (g *Gateway) markPendingByMachine(machineID, traceID string, state pendingState, uid, nick, errMsg string) {
	if machineID == "" && traceID == "" {
		return
	}
	g.loginMu.Lock()
	defer g.loginMu.Unlock()
	for _, pl := range g.logins {
		if pl.machineID == machineID || (traceID != "" && machineTraceID(pl.machineID, pl.deviceID) == traceID) {
			pl.state = state
			if uid != "" {
				pl.uid = uid
			}
			if nick != "" {
				pl.nickname = nick
			}
			if errMsg != "" {
				pl.errMsg = errMsg
			}
			return
		}
	}
}

// getPendingByTrace 用 loginTraceID（= machineTraceID(machine,device)）反查 pending。
func (g *Gateway) getPendingByTrace(traceID string) (*pendingLogin, bool) {
	if traceID == "" {
		return nil, false
	}
	g.loginMu.Lock()
	defer g.loginMu.Unlock()
	for _, pl := range g.logins {
		if machineTraceID(pl.machineID, pl.deviceID) == traceID {
			return pl, true
		}
	}
	return nil, false
}

// getPending 取 pending（不存在或已过期 false）。pending TTL 10 分钟。
func (g *Gateway) getPending(id string) (*pendingLogin, bool) {
	if id == "" {
		return nil, false
	}
	g.loginMu.Lock()
	defer g.loginMu.Unlock()
	pl, ok := g.logins[id]
	if !ok {
		return nil, false
	}
	if time.Since(pl.createdAt) > 10*time.Minute {
		delete(g.logins, id)
		return nil, false
	}
	return pl, true
}

// randomHex 生成 n 字节随机 hex（2n hex 字符）。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// mkdirAll 包装 os.MkdirAll。
func mkdirAll(dir string) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// authorizeRender 渲染 /authorize 回调后的浏览器可见页面（非 JSON）。
// 极简：一个标题 + 一段说明 + 自动关闭尝试。
func authorizeRender(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	html := `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>TRAE 登录</title><style>` +
		`body{background:#0f1115;color:#e6e9ef;font-family:"Segoe UI","Microsoft YaHei",system-ui,sans-serif;` +
		`display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}` +
		`.box{max-width:480px;padding:32px;text-align:center}` +
		`h1{font-size:20px;font-weight:600;margin:0 0 12px}` +
		`p{color:#8a93a6;line-height:1.6;margin:0 0 8px;word-break:break-all}` +
		`</style></head><body><div class="box"><h1>` + title + `</h1><p>` + detail + `</p>` +
		`<p style="margin-top:20px;font-size:12px">窗口可关闭并返回 workbuddy 面板。</p>` +
		`<script>try{setTimeout(function(){window.close()},3000);}catch(e){}</script>` +
		`</div></body></html>`
	_, _ = w.Write([]byte(html))
}
