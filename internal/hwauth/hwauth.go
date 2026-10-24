// Package hwauth 华为云身份底座：DPoP proof（RFC 9449）+ SDK-HMAC-SHA256 /
// V11-HMAC-SHA256 请求签名。CodeArts 与 OfficeAce 两渠道共用。
//
// 移植自 agent2api（Rust），签名分两条独立路径（金向量钉死，见 hwauth_test.go）：
//
//   - CodeArts SDK（codearts/signer.rs）：未保留集是 JS encodeURIComponent 的那组
//     （!'()* 字面量）、query 按 Go ParseQuery 语义、注入 X-Domain-Id、GET 恒签
//     sha256("")、canonicalURI 强制补尾斜杠。
//   - OfficeAce（officeace/signer.rs，SDK 与 V11 共用规范化）：未保留集更严
//     （!'()* 也转义）、query 按 WHATWG 口径（+ 折空格）、注入 X-Project-ID（两算法
//     都注入）、空体签 sha256("")。V11 派生密钥按小写 hex 字符串参与 HMAC（不是
//     原始 32 字节）—— 照抄 CodeArts 那套会在这三处算错、上游只回 401。
//
// DPoP：codearts/dpop.rs —— ES256 JWS，头部嵌公钥 JWK，私钥随凭据落盘（换一把去
// 刷新会被 STS 判 InvalidDPoPHeader）。
package hwauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// DPoP
// ---------------------------------------------------------------------------

// Jwk P-256 JWK。d 是私钥标量的 base64url（32 字节定长）；序列化时私钥侧带 d、
// 公钥侧不带（omitempty）。
type Jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
}

// KeyPair DPoP 密钥对（落盘形态对齐 agent2api credentials.rs DpopKeyPair：
// 私钥那半必须与 refresh_token 一起保存，丢了只能重新登录）。
type KeyPair struct {
	PrivateKeyJwk Jwk `json:"private_key_jwk"`
	PublicKeyJwk  Jwk `json:"public_key_jwk"`
}

// GenerateKeyPair 生成一把 P-256 密钥对。
func GenerateKeyPair() (KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("生成 DPoP 密钥失败: %w", err)
	}
	return KeyPair{
		PrivateKeyJwk: privateJwkOf(key),
		PublicKeyJwk:  publicJwkOf(&key.PublicKey),
	}, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// pad32 左填充到 32 字节（EC 坐标/标量去掉前导零后必须补齐，不补会被判成非法点）。
func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func publicJwkOf(pub *ecdsa.PublicKey) Jwk {
	return Jwk{Kty: "EC", Crv: "P-256",
		X: b64url(pad32(pub.X.Bytes())), Y: b64url(pad32(pub.Y.Bytes()))}
}

func privateJwkOf(key *ecdsa.PrivateKey) Jwk {
	j := publicJwkOf(&key.PublicKey)
	j.D = b64url(pad32(key.D.Bytes()))
	return j
}

// LoadKeyPair 从落盘形态恢复私钥；落盘公钥存在且与私钥不一致时报错
//（宁可不签，也不签一份头里公钥与签名私钥对不上的 proof —— agent2api dpop.rs 同款）。
func LoadKeyPair(pair KeyPair) (*ecdsa.PrivateKey, error) {
	priv := pair.PrivateKeyJwk
	if priv.Kty != "EC" || priv.Crv != "P-256" || strings.TrimSpace(priv.D) == "" {
		return nil, fmt.Errorf("DPoP 私钥不完整（需要 EC / P-256 / d）")
	}
	d, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(priv.D))
	if err != nil || len(d) == 0 {
		return nil, fmt.Errorf("DPoP 私钥不是合法的 base64url")
	}
	key := new(ecdsa.PrivateKey)
	key.D = new(big.Int).SetBytes(d)
	key.Curve = elliptic.P256()
	key.PublicKey.Curve = key.Curve
	key.PublicKey.X, key.PublicKey.Y = key.Curve.ScalarBaseMult(key.D.Bytes())
	if strings.TrimSpace(pair.PublicKeyJwk.X) != "" {
		got, err1 := base64.RawURLEncoding.DecodeString(strings.TrimSpace(pair.PublicKeyJwk.X))
		gotY, err2 := base64.RawURLEncoding.DecodeString(strings.TrimSpace(pair.PublicKeyJwk.Y))
		want := publicJwkOf(&key.PublicKey)
		wantX, _ := base64.RawURLEncoding.DecodeString(want.X)
		wantY, _ := base64.RawURLEncoding.DecodeString(want.Y)
		if err1 != nil || err2 != nil || string(got) != string(wantX) || string(gotY) != string(wantY) {
			return nil, fmt.Errorf("DPoP 公私钥不一致，请重新登录")
		}
	}
	return key, nil
}

// DPoPProof 现签一份 proof。
//
// method 与 endpoint 必须与即将发出的请求逐字一致（上游会比对，对不上就是 401）；
// htu 用完整地址，不带 query、不带 fragment。iat 秒级、jti 32 字节随机 hex。
func DPoPProof(key *ecdsa.PrivateKey, method, endpoint string, now time.Time) (string, error) {
	header := map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": publicJwkOf(&key.PublicKey)}
	jti := make([]byte, 32)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	payload := map[string]any{
		"htm": strings.ToUpper(method),
		"htu": endpoint,
		"iat": now.Unix(),
		"jti": hex.EncodeToString(jti),
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(pb)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("DPoP 签名失败: %w", err)
	}
	sig := make([]byte, 64)
	copy(sig[:32], pad32(r.Bytes()))
	copy(sig[32:], pad32(s.Bytes()))
	return signingInput + "." + b64url(sig), nil
}

// ---------------------------------------------------------------------------
// 共用件
// ---------------------------------------------------------------------------

const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func hmacSHA256Hex(key []byte, msg string) string {
	return hex.EncodeToString(hmacSHA256Raw(key, msg))
}

// hmacSHA256Raw 返回原始 32 字节摘要（V11 派生链用）。
func hmacSHA256Raw(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}

// sdkDate UTC 的 yyyyMMddTHHmmssZ。
func sdkDate(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

// jsUnreserved JS encodeURIComponent 的未保留集（!'()* 字面量保留）。
func jsUnreserved(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' || b == '!' || b == '~' || b == '*' ||
		b == '\'' || b == '(' || b == ')'
}

// strictUnreserved OfficeAce 的未保留集（!'()* 一并转义）。
func strictUnreserved(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' || b == '~'
}

// encodeComponent 按未保留集做百分号编码（大写 hex）。
func encodeComponent(b []byte, keep func(byte) bool) string {
	const hexdigits = "0123456789ABCDEF"
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if keep(c) {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hexdigits[c>>4])
			sb.WriteByte(hexdigits[c&0x0f])
		}
	}
	return sb.String()
}

// headerBag 大小写无关头集合：先到先得位置、后到覆盖值。
type headerBag struct{ items [][2]string }

func (h *headerBag) set(name, value string) {
	lower := strings.ToLower(name)
	for i, kv := range h.items {
		if strings.ToLower(kv[0]) == lower {
			h.items[i][1] = value
			return
		}
	}
	h.items = append(h.items, [2]string{name, value})
}

func (h *headerBag) contains(name string) bool {
	lower := strings.ToLower(name)
	for _, kv := range h.items {
		if strings.ToLower(kv[0]) == lower {
			return true
		}
	}
	return false
}

func (h *headerBag) value(name string) string {
	lower := strings.ToLower(name)
	for _, kv := range h.items {
		if strings.ToLower(kv[0]) == lower {
			return kv[1]
		}
	}
	return ""
}

// splitURL 拆 authority / path / raw query（fragment 丢弃）。
func splitURL(rawURL string) (authority, path, rawQuery string, err error) {
	var rest string
	if r, ok := strings.CutPrefix(rawURL, "https://"); ok {
		rest = r
	} else if r, ok := strings.CutPrefix(rawURL, "http://"); ok {
		rest = r
	} else {
		return "", "", "", fmt.Errorf("地址缺少 scheme: %s", rawURL)
	}
	authority = rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
		rest = rest[i:]
	} else {
		rest = ""
	}
	if authority == "" {
		return "", "", "", fmt.Errorf("地址缺少主机名: %s", rawURL)
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	path, rawQuery = rest, ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		path, rawQuery = rest[:i], rest[i+1:]
	}
	return
}

// canonicalURI 逐段解码再按未保留集重编码，强制补尾斜杠（空路径给 /）。
func canonicalURI(path string, keep func(byte) bool) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = encodeComponent(percentDecode(seg, false), keep)
	}
	out := strings.Join(segs, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// percentDecode %XX 解字节；plus=true 时 + 折空格。坏转义按原样保留该字节
//（宽松；签名调用方的 URL 都是合法转义，OfficeAce 的 decode_component 也是这个语义）。
func percentDecode(s string, plus bool) []byte {
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '%' && i+2 < len(b)+1 && i+2 <= len(b)-1 {
			hi, ok1 := unhex(b[i+1])
			lo, ok2 := unhex(b[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		if b[i] == '+' && plus {
			out = append(out, ' ')
			continue
		}
		out = append(out, b[i])
	}
	return out
}

func unhex(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

// SigningCredential 签名所需凭据。SecurityToken / DomainID / ProjectID 为空时
// 完全不进头集合（不是进一个空值），否则签名头集合与上游收到的不一致。
type SigningCredential struct {
	AccessKeyID   string
	SecretKey     string
	SecurityToken string
	DomainID      string // CodeArts（X-Domain-Id）
	ProjectID     string // OfficeAce（X-Project-ID，SDK 与 V11 都注入）
}

func (c SigningCredential) valid() bool { return c.AccessKeyID != "" && c.SecretKey != "" }

// Signed 一次签名的产出：应当发出去的完整头集合（含 Authorization）+ 排障中间量。
type Signed struct {
	Headers       [][2]string
	CanonicalReq  string
	StringToSign  string
	SignedHeaders string
}

// canonicalHeaders 规范头视图：名字小写、按名排序；SDK 值 trim、OfficeAce 值把
// 内部连续空白折成一个空格（JS replace(/\s+/g,' ')）。
func canonicalHeaders(bag *headerBag, foldSpaces bool) (view, signedList string) {
	type kv struct{ k, v string }
	canon := make([]kv, 0, len(bag.items))
	for _, h := range bag.items {
		v := strings.TrimSpace(h[1])
		if foldSpaces {
			v = strings.Join(strings.Fields(h[1]), " ")
		}
		canon = append(canon, kv{strings.ToLower(h[0]), v})
	}
	sort.Slice(canon, func(i, j int) bool { return canon[i].k < canon[j].k })
	var chb, shb strings.Builder
	for _, c := range canon {
		chb.WriteString(c.k + ":" + c.v + "\n")
		shb.WriteString(c.k + ";")
	}
	return chb.String(), strings.TrimSuffix(shb.String(), ";")
}

// signSDK CodeArts SDK-HMAC-SHA256（见包注释的三条易错细节）。
func signSDK(method, rawURL string, headers [][2]string, body []byte, cred SigningCredential, includeHost bool) (*Signed, error) {
	if !cred.valid() {
		return nil, fmt.Errorf("凭据不完整：access key 与 secret key 都得有")
	}
	authority, path, rawQuery, err := splitURL(rawURL)
	if err != nil {
		return nil, err
	}
	bag := &headerBag{}
	for _, kv := range headers {
		bag.set(kv[0], kv[1])
	}
	if cred.SecurityToken != "" {
		bag.set("X-Security-Token", cred.SecurityToken)
	}
	if cred.DomainID != "" {
		bag.set("X-Domain-Id", cred.DomainID)
	}
	if !bag.contains("X-Sdk-Date") {
		bag.set("X-Sdk-Date", sdkDate(time.Now()))
	}
	if includeHost && !bag.contains("host") {
		bag.set("Host", authority)
	}
	date := bag.value("X-Sdk-Date")
	view, signedHeaders := canonicalHeaders(bag, false)

	// 只有 PUT/PATCH/POST 算「带体」，其余恒签空体摘要（agent2api signer.rs 同款）。
	payloadHash := emptyBodySHA256
	switch strings.ToUpper(method) {
	case "PUT", "PATCH", "POST":
		payloadHash = sha256Hex(body)
	}

	cReq := strings.Join([]string{
		strings.ToUpper(method),
		canonicalURI(path, jsUnreserved),
		canonicalQueryGo(rawQuery, jsUnreserved),
		view,
		signedHeaders,
		payloadHash,
	}, "\n")
	st := strings.Join([]string{"SDK-HMAC-SHA256", date, sha256Hex([]byte(cReq))}, "\n")
	sig := hmacSHA256Hex([]byte(cred.SecretKey), st)
	bag.set("Authorization", "SDK-HMAC-SHA256 Access="+cred.AccessKeyID+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
	return &Signed{Headers: bag.items, CanonicalReq: cReq, StringToSign: st, SignedHeaders: signedHeaders}, nil
}

// signOfficeAce OfficeAce 的 SDK 与 V11 共用规范化（strict 未保留集 + WHATWG
// query + X-Project-ID 注入 + 空体签 sha256("")）；v11=true 时走 V11 派生链。
func signOfficeAce(alg, method, rawURL string, headers [][2]string, body []byte,
	cred SigningCredential, region string, v11 bool) (*Signed, error) {
	if !cred.valid() {
		return nil, fmt.Errorf("凭据不完整：access key 与 secret key 都得有")
	}
	authority, path, rawQuery, err := splitURL(rawURL)
	if err != nil {
		return nil, err
	}
	bag := &headerBag{}
	for _, kv := range headers {
		bag.set(kv[0], kv[1])
	}
	if cred.SecurityToken != "" {
		bag.set("X-Security-Token", cred.SecurityToken)
	}
	if cred.ProjectID != "" {
		bag.set("X-Project-ID", cred.ProjectID)
	}
	if !bag.contains("X-Sdk-Date") {
		bag.set("X-Sdk-Date", sdkDate(time.Now()))
	}
	if !bag.contains("host") {
		bag.set("Host", authority)
	}
	date := bag.value("X-Sdk-Date")
	view, signedHeaders := canonicalHeaders(bag, true)

	payloadHash := emptyBodySHA256
	if len(body) > 0 {
		payloadHash = sha256Hex(body)
	}

	cReq := strings.Join([]string{
		strings.ToUpper(method),
		canonicalURI(path, strictUnreserved),
		canonicalQueryWHATWG(rawQuery, strictUnreserved),
		view,
		signedHeaders,
		payloadHash,
	}, "\n")

	var st, sig, authz string
	if v11 {
		scope := date[:min(8, len(date))] + "/" + region + "/apic"
		st = strings.Join([]string{alg, date, scope, sha256Hex([]byte(cReq))}, "\n")
		step1 := hmacSHA256Raw([]byte(cred.AccessKeyID), cred.SecretKey)
		seed := append([]byte(scope), 0x01)
		derived := hmacSHA256Raw(step1, string(seed))[:32]
		// ⚠️ 按小写 hex 字符串当 HMAC key（不是那 32 个原始字节）——
		// officeace2api 与 opencode-officeace-auth 两份实现一致，必须复刻。
		sig = hmacSHA256Hex([]byte(hex.EncodeToString(derived)), st)
		authz = alg + " Credential=" + cred.AccessKeyID + "/" + scope +
			", SignedHeaders=" + signedHeaders + ", Signature=" + sig
	} else {
		st = strings.Join([]string{alg, date, sha256Hex([]byte(cReq))}, "\n")
		sig = hmacSHA256Hex([]byte(cred.SecretKey), st)
		authz = alg + " Access=" + cred.AccessKeyID +
			", SignedHeaders=" + signedHeaders + ", Signature=" + sig
	}
	bag.set("Authorization", authz)
	return &Signed{Headers: bag.items, CanonicalReq: cReq, StringToSign: st, SignedHeaders: signedHeaders}, nil
}

// SignSDK CodeArts 的 SDK-HMAC-SHA256。
func SignSDK(method, url string, headers [][2]string, body []byte, cred SigningCredential, includeHost bool) (*Signed, error) {
	return signSDK(method, url, headers, body, cred, includeHost)
}

// SignOfficeAceSDK OfficeAce 的 SDK-HMAC-SHA256（规范化与 V11 同一套）。
func SignOfficeAceSDK(method, url string, headers [][2]string, body []byte, cred SigningCredential) (*Signed, error) {
	return signOfficeAce("SDK-HMAC-SHA256", method, url, headers, body, cred, "", false)
}

// SignV11 OfficeAce 的 V11-HMAC-SHA256（region 固定 cn-southwest-2，服务名 apic）。
func SignV11(method, url string, headers [][2]string, body []byte, cred SigningCredential, region string) (*Signed, error) {
	return signOfficeAce("V11-HMAC-SHA256", method, url, headers, body, cred, region, true)
}

// canonicalQueryGo Go ParseQuery 语义：& 切分；含 ; 的对丢弃；空对丢弃；
// 坏 % 丢该对；+ 折空格；按键（编码后）再按值字节排序。
func canonicalQueryGo(raw string, keep func(byte) bool) string {
	if raw == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(raw, "&") {
		if part == "" || strings.Contains(part, ";") {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		kb, ok1 := strictQueryUnescape(k)
		vb, ok2 := strictQueryUnescape(v)
		if !ok1 || !ok2 {
			continue
		}
		pairs = append(pairs, pair{encodeComponent(kb, keep), encodeComponent(vb, keep)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(p.k + "=" + p.v)
	}
	return sb.String()
}

// canonicalQueryWHATWG URLSearchParams 口径：+ 折空格、%XX 解码后重编、
// 按 (键,值) 排序。
func canonicalQueryWHATWG(raw string, keep func(byte) bool) string {
	if raw == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, pair{string(percentDecode(k, true)), string(percentDecode(v, true))})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(encodeComponent([]byte(p.k), keep) + "=" + encodeComponent([]byte(p.v), keep))
	}
	return sb.String()
}

// strictQueryUnescape Go net/url unescape 的 query 语义：坏 % 整对作废。
func strictQueryUnescape(s string) ([]byte, bool) {
	b := []byte(s)
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '%' {
			if i+2 > len(b)-1 {
				return nil, false
			}
			hi, ok1 := unhex(b[i+1])
			lo, ok2 := unhex(b[i+2])
			if !ok1 || !ok2 {
				return nil, false
			}
			out = append(out, hi<<4|lo)
			i += 2
			continue
		}
		if b[i] == '+' {
			out = append(out, ' ')
			continue
		}
		out = append(out, b[i])
	}
	return out, true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
