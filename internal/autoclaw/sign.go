// sign.go 两条链的请求头构造：userapi 签名链（X-Auth-Sign MD5）与对话链品牌头。
// 两条链刻意不一致（X-Harness-Type / X-Version / X-Tm 各不同），别统一——
// 每一处差异都是上游指纹门控（agent2api adapter.rs / refresh.rs 实测标注）。
package autoclaw

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

const (
	// authAppID / authAppKey 客户端内嵌签名指纹（硬编码，不可配置化）。
	authAppID = "100003"
	authAppKey = "38d2391985e2369a5fb8227d8e6cd5e5"

	// chatVersion 对话链 X-Version（上游按它识别客户端版本走不同兼容路径）。
	chatVersion = "1.17.8"
	// userAPIVersion userapi 链 X-Version（模型目录按它版本门控：过时静默少模型——
	// 国内版缺 zai_glm-5.3-flash；不报错）。
	userAPIVersion = "1.18.5"
)

// authSign MD5("{appId}&{秒级ts}&{appKey}") 小写 hex（Node digest('hex') 等价）。
// 时间戳单位是秒（源码 now_ms/1000）——与毫秒搞混稳定 400002。
func authSign(tsSeconds int64) string {
	sum := md5.Sum([]byte(authAppID + "&" + strconv.FormatInt(tsSeconds, 10) + "&" + authAppKey))
	return hex.EncodeToString(sum[:])
}

// signedAuthHeaders userapi 链签名头（refresh/目录/积分/签到/登录；refresh.rs:242-284）。
// token 传空串 = 匿名（登录链第一跳）。
func signedAuthHeaders(token string) [][2]string {
	ts := time.Now().Unix()
	hs := [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "*/*"},
		{"X-Version", userAPIVersion},
		{"X-Product", "autoclaw"},
		{"X-Client-Type", "pc"},
		// userapi 链必须带 X-Harness-Type: zcode——与 X-Auth-Sign 一起构成官方
		// 客户端指纹，删掉风险方向是 400002（与对话链刻意相反）。
		{"X-Harness-Type", "zcode"},
		{"X-Tm", "win"},
		{"X-Lang", "zh-CN"},
		{"X-Channel", "official"},
		{"X-Auth-Appid", authAppID},
		{"X-Auth-TimeStamp", strconv.FormatInt(ts, 10)},
		{"X-Auth-Sign", authSign(ts)},
		{"X-Trace-Id", newTraceID()},
	}
	if token != "" {
		hs = append(hs, [][2]string{{"authorization", "Bearer " + token}}...)
	}
	return hs
}

// chatHeaders 对话链品牌头（adapter.rs:551-561 逐字）。全列表无 User-Agent 要求、
// 无 X-Harness-Type（发了 403 pay-view/810001——方向与 userapi 链相反）。
func chatHeaders(token, routeModel, requestID string, extra [][2]string) [][2]string {
	hs := [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "*/*"},
		{"X-Product", "autoclaw"},
		{"X-Client-Type", "pc"},
		// X-Tm 平台绑定：上游绑为 Windows 的账号收到 "linux" 一律 403 非空体
		// {"message":"forbidden"}——服务器上恒发 win（adapter.rs:563-585）。
		{"X-Tm", "win"},
		{"X-Version", chatVersion},
		{"X-Lang", "zh-CN"},
		{"X-Channel", "official"},
		// x_trace_id（下划线）是客户端指纹的一部分，与 X-Request-Id 并存，各自照抄。
		{"x_trace_id", "autoclaw-desktop"},
		// 对话认证头是 X-Authorization 不是 Authorization——发错稳定 401。
		{"X-Authorization", "Bearer " + token},
		{"X-Request-Id", requestID},
		// 完整路由 ID（带通道前缀）决定计费通道；body.model 是剥前缀模型 ID。
		{"X-Request-Model", routeModel},
	}
	for _, kv := range extra {
		if kv[1] != "" {
			hs = append(hs, kv)
		}
	}
	return hs
}

// newRequestID 每次对话请求新生成（32 hex）。
func newRequestID() string { return randomHex(16) }

// newTraceID 每次请求新生成（32 hex）。
func newTraceID() string { return randomHex(16) }

// NewDeviceID 32 字节随机 → 64 位 hex（官方是 ed25519 公钥 SHA-256，
// 服务端只当不透明串；两跳登录必须同一 device_id）。
func NewDeviceID() string { return randomHex(32) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// DPAPI + AES-GCM safeStorage 解密（桌面端 auth.json 的 enc: 值；Windows 专属）
// ---------------------------------------------------------------------------

// decryptSafeStorage 解 Chromium safeStorage 密文（crypto.rs:226-277）：
// base64 → 前 3 字节 "v10" → 前 12 字节 nonce、尾 16 字节 tag（Postfix）、
// 中间密文 → AES-256-GCM 无 AAD。密钥来自 Local State 的 DPAPI 解密。
func decryptSafeStorage(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("enc: 值不是合法 base64: %w", err)
	}
	if len(raw) < 3 || string(raw[:3]) != "v10" {
		return "", fmt.Errorf("enc: 值不是 v10 形态的 safeStorage 密文")
	}
	payload := raw[3:]
	if len(payload) <= 12+16 {
		return "", fmt.Errorf("enc: 密文长度异常（%d 字节，应 > 28）", len(payload))
	}
	key, err := dpapiLocalStateKey()
	if err != nil {
		return "", err
	}
	nonce := payload[:12]
	tag := payload[len(payload)-16:]
	ciphertext := payload[12 : len(payload)-16]
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	// Go 的 Seal/Open 都是 tag 后缀（Postfix）形态，与 Chromium 一致。
	plain, err := gcm.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("safeStorage 解密失败（密钥或密文不匹配）: %w", err)
	}
	return string(plain), nil
}

// dpapiLocalStateKey 读 %APPDATA%/AutoClaw/Local State → os_crypt.encrypted_key
// → base64 → 去 5 字节 "DPAPI" 前缀 → Win32 CryptUnprotectData → 32 字节 AES 密钥。
func dpapiLocalStateKey() ([]byte, error) {
	appdata := windowsAPPDATA()
	if appdata == "" {
		return nil, fmt.Errorf("DPAPI 解密仅支持 Windows 平台（非 Windows 请粘贴明文 token）")
	}
	raw, err := osReadFile(appdata + `\AutoClaw\Local State`)
	if err != nil {
		return nil, fmt.Errorf("读不到桌面端 Local State（%w）：请直接粘贴明文 token（客户端开发者工具 Network 里 X-Authorization 的值）", err)
	}
	var meta struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.OsCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("Local State 里没有 os_crypt.encrypted_key")
	}
	wrapped, err := base64.StdEncoding.DecodeString(meta.OsCrypt.EncryptedKey)
	if err != nil || len(wrapped) < 5 || string(wrapped[:5]) != "DPAPI" {
		return nil, fmt.Errorf("encrypted_key 不是 DPAPI 包裹形态")
	}
	key, err := cryptUnprotectData(wrapped[5:])
	if err != nil {
		return nil, fmt.Errorf("DPAPI 解密失败（须在本机同一 Windows 用户下运行）: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("DPAPI 解出的密钥不是 32 字节（%d）", len(key))
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Windows 系统调用（DPAPI CryptUnprotectData）
// ---------------------------------------------------------------------------

// windowsAPPDATA 取 %APPDATA%（非 Windows 返回空）。
func windowsAPPDATA() string {
	if isWindows() {
		return osGetenv("APPDATA")
	}
	return ""
}

// cryptUnprotectData 调 Win32 CryptUnprotectData（CurrentUser，flags=0）。
func cryptUnprotectData(data []byte) ([]byte, error) {
	if !isWindows() {
		return nil, fmt.Errorf("DPAPI 解密仅支持 Windows 平台")
	}
	return cryptUnprotectDataWin(data)
}

// 平台桥（真实实现见 zsys_windows.go / zsys_other.go；此处声明避免循环依赖）。
func isWindows() bool { return unsafe.Sizeof(uintptr(0)) == 8 && goosWindows }

// osReadFile / osGetenv 桥接（测试可注入）。
var osReadFile = func(p string) ([]byte, error) { return readFileBridge(p) }
var osGetenv = func(k string) string { return getenvBridge(k) }
