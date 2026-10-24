// Package cosy 实现 Qoder 系列渠道（Qoder / QoderWork）共用的 COSY 协议原语：
// RSA 包裹会话密钥、AES-128-CBC 身份加密、MD5 请求签名、私有 base64 body 编码，
// 以及配套的通用账号池与错误分类。
//
// 同一套算法在 three-party 参考实现里出现过两次（纯 Go），除 payload 字段序与
// 头部常量外完全一致，故抽为本包供各渠道复用。
package cosy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ServerPublicKeyPEM Qoder 系桌面客户端内置 RSA 公钥（1024 位 PKIX）。
// Qoder 与 QoderWork 参考实现中该常量逐字符相同，故共用。
const ServerPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var (
	serverPubKey *rsa.PublicKey
	serverKeyErr error
)

func init() {
	block, _ := pem.Decode([]byte(ServerPublicKeyPEM))
	if block == nil {
		serverKeyErr = errors.New("cosy: bad server public key PEM")
		return
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		serverKeyErr = err
		return
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok {
		serverKeyErr = errors.New("cosy: server public key is not RSA")
		return
	}
	serverPubKey = pub
}

// Fingerprint 账号的 COSY 持久机器指纹（首次使用后落盘，不可随请求漂移）。
type Fingerprint struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
}

// Complete 三项齐全才可用（缺失时签名直接失败）。
func (f Fingerprint) Complete() bool {
	return f.MachineID != "" && f.MachineToken != "" && f.MachineType != ""
}

// NewFingerprint 生成一套新指纹。口径与参考实现一致：
// MachineID=uuid4；MachineToken=base64(uuid4+uuid4 前 50 字符)；
// MachineType=uuid4 去横线前 18 位。
func NewFingerprint() Fingerprint {
	tok := UUID() + UUID()
	return Fingerprint{
		MachineID:    UUID(),
		MachineToken: base64.RawURLEncoding.EncodeToString([]byte(tok[:50])),
		MachineType:  strings.ReplaceAll(UUID(), "-", "")[:18],
	}
}

// Identity info/encrypt_user_info 明文字段（AES-CBC 加密后随签名上行）。
type Identity struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// PayloadOrder payload 序列化顺序。签名覆盖的是 base64(payload) 字节串，
// 选错即全量 403，故两侧各按参考实现固定。
type PayloadOrder int

const (
	// OrderSorted 按 key 字典序（QoderWork / qoderwork2api 口径）。
	OrderSorted PayloadOrder = iota
	// OrderStruct 按 version,requestId,info,cosyVersion,ideVersion 声明序
	//（Qoder 桌面端 / API-Console 口径）。
	OrderStruct
)

// Payload 参与签名的 payload 参数。
type Payload struct {
	Order       PayloadOrder
	Version     string // json "version"，恒 "v1"
	CosyVersion string // json "cosyVersion"，与 cosy-version 头同值
	IDEVersion  string // json "ideVersion"，桌面端留空
	RequestID   string
}

// Session 单次/单账号的 COSY 签名会话（每次请求新建：内嵌的时间戳与 requestId）。
type Session struct {
	fp       Fingerprint
	tempKey  []byte
	CosyKey  string // base64(RSA(tempKey))
	Info     string // base64(AES-CBC(identity))
	identity Identity
}

// NewSession 用持久指纹与身份信息构建签名会话。
// dt/drt 变化（refresh）后需重建——identity 内嵌这两个 token。
func NewSession(fp Fingerprint, id Identity) (*Session, error) {
	if serverKeyErr != nil {
		return nil, serverKeyErr
	}
	if !fp.Complete() {
		return nil, errors.New("cosy: missing machine fingerprint")
	}
	tempKey := []byte(hexShort(16)) // 16 个 ASCII hex 字符，AES-128
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, tempKey)
	if err != nil {
		return nil, fmt.Errorf("cosy: wrap session key: %w", err)
	}
	plain, err := json.Marshal(identityWire(id))
	if err != nil {
		return nil, err
	}
	sealed, err := aesCBCEncrypt(plain, tempKey)
	if err != nil {
		return nil, err
	}
	return &Session{
		fp:       fp,
		tempKey:  tempKey,
		CosyKey:  base64.StdEncoding.EncodeToString(wrapped),
		Info:     base64.StdEncoding.EncodeToString(sealed),
		identity: id,
	}, nil
}

// Fingerprint 会话使用的机器指纹。
func (s *Session) Fingerprint() Fingerprint { return s.fp }

// Identity 会话内嵌的身份（含 dt/drt）。
func (s *Session) Identity() Identity { return s.identity }

// DeriveRuntime 以身份明文派生可持久复用的运行时字段（CosyKey=base64(RSA(tempKey))，
// Info=base64(AES-CBC(identity))）。Qoder(qoder.sh) 侧 CLI 在登录时派生一次并随
// 凭证持久复用（每请求新建会让网关侧会话漂移），token 轮换后需重派生。
func DeriveRuntime(id Identity) (cosyKey, info string, err error) {
	if serverKeyErr != nil {
		return "", "", serverKeyErr
	}
	tempKey := []byte(hexShort(16))
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, serverPubKey, tempKey)
	if err != nil {
		return "", "", fmt.Errorf("cosy: wrap runtime key: %w", err)
	}
	plain, err := json.Marshal(identityWire(id))
	if err != nil {
		return "", "", err
	}
	sealed, err := aesCBCEncrypt(plain, tempKey)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(wrapped),
		base64.StdEncoding.EncodeToString(sealed), nil
}

// NewSessionFromRuntime 以已派生的运行时字段构建会话（配对 DeriveRuntime 使用）。
func NewSessionFromRuntime(fp Fingerprint, id Identity, cosyKey, info string) (*Session, error) {
	if !fp.Complete() {
		return nil, errors.New("cosy: missing machine fingerprint")
	}
	if strings.TrimSpace(cosyKey) == "" || strings.TrimSpace(info) == "" {
		return nil, errors.New("cosy: incomplete runtime fields")
	}
	return &Session{fp: fp, CosyKey: cosyKey, Info: info, identity: id}, nil
}

// identityWire 是 identity 的输出形（声明序即字段序；服务端解密后按 key 解析，
// 顺序本身无协议含义，保留 struct 仅为零分配且稳定可读）。
type identityWire struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// PayloadBase64 序列化并 base64 编码 payload（签名首段）。
func (s *Session) PayloadBase64(p Payload) (string, error) {
	var raw []byte
	switch p.Order {
	case OrderStruct:
		var v struct {
			Version     string `json:"version"`
			RequestID   string `json:"requestId"`
			Info        string `json:"info"`
			CosyVersion string `json:"cosyVersion"`
			IDEVersion  string `json:"ideVersion"`
		}
		v.Version, v.RequestID, v.Info = p.Version, p.RequestID, s.Info
		v.CosyVersion, v.IDEVersion = p.CosyVersion, p.IDEVersion
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		raw = b
	default:
		raw = jsonSortedCompact(map[string]string{
			"cosyVersion": p.CosyVersion,
			"ideVersion":  p.IDEVersion,
			"info":        s.Info,
			"requestId":   p.RequestID,
			"version":     p.Version,
		})
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// Authorization 计算单次请求的 Authorization 头值：
//
//	Bearer COSY.<base64(payload)>.<hex(md5(payload\ncosyKey\ndate\nbody\npath))>
//
// body 为**编码后**的 body（与上行字节一致）；signedPath 见 SignedPath。
func (s *Session) Authorization(body []byte, signedPath string, p Payload, now time.Time) (string, error) {
	payloadB64, err := s.PayloadBase64(p)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.Grow(len(payloadB64) + len(s.CosyKey) + len(signedPath) + len(body) + 16)
	sb.WriteString(payloadB64)
	sb.WriteByte('\n')
	sb.WriteString(s.CosyKey)
	sb.WriteByte('\n')
	sb.WriteString(fmt.Sprintf("%d", now.Unix()))
	sb.WriteByte('\n')
	sb.Write(body)
	sb.WriteByte('\n')
	sb.WriteString(signedPath)
	sum := md5.Sum([]byte(sb.String()))
	return "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:]), nil
}

// SignedPath 取 URL Path 并去掉 /algo 前缀（query 不参与签名）。
func SignedPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return strings.TrimPrefix(u.Path, "/algo")
}

// nowFunc 返回当前时间。抽成变量仅为测试可替换。
var nowFunc = func() time.Time { return time.Now() }

// unixSeconds 渲染 Cosy-Date 头（十进位秒）。
func unixSeconds(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// Traceparent 按 W3C Trace Context 渲染请求 ID：版本 00、32 位小写 hex trace id、
// 16 位 parent span id、flags 01。
//
// 调用方传入的是它为本次请求生成的 UUID，其中的横线与可能的大写字母都不符合
// trace-context 语法，故先剥离非 hex 字符并小写化。全零 id 被 W3C 禁止，回落到
// 领域分隔的 SHA-256 派生值，保证同一 requestId 始终映射同一 traceparent。
func Traceparent(requestID string) string {
	digits := make([]byte, 0, 32)
	for i := 0; i < len(requestID) && len(digits) < 32; i++ {
		switch char := requestID[i]; {
		case char >= '0' && char <= '9', char >= 'a' && char <= 'f':
			digits = append(digits, char)
		case char >= 'A' && char <= 'F':
			digits = append(digits, char+('a'-'A'))
		}
	}
	for len(digits) < 32 {
		digits = append(digits, '0')
	}
	traceID := string(digits)
	if strings.Trim(traceID, "0") == "" || strings.Trim(traceID[:16], "0") == "" {
		digest := sha256.Sum256([]byte("cosy:traceparent:" + requestID))
		traceID = hex.EncodeToString(digest[:16])
		if strings.Trim(traceID[:16], "0") == "" {
			traceID = "1" + traceID[1:]
		}
	}
	return "00-" + traceID + "-" + traceID[:16] + "-01"
}

// UUID 生成 v4 UUID 字符串。
func UUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hexShort 生成 n 字符随机 hex（tempKey 用，与桌面端 uuid.hex[:16] 对齐）。
func hexShort(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// jsonSortedCompact 按 key 字典序、无空白序列化。
func jsonSortedCompact(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// aesCBCEncrypt AES-128-CBC，key 兼 iv，PKCS7 padding。
func aesCBCEncrypt(plain, tempKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(tempKey)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, tempKey).CryptBlocks(out, padded)
	return out, nil
}
