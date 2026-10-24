package hwauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 签名金向量（源自 agent2api signer_vectors.json，由 officeace2api 真实实现产出）
// ---------------------------------------------------------------------------

var vectorCred = SigningCredential{
	AccessKeyID:   "AKTESTACCESSKEY0001",
	SecretKey:     "SKTESTSECRETKEY0001000000000000000000",
	SecurityToken: "STTESTSECURITYTOKEN0001",
	ProjectID:     "pid-00000000000000000000000000000001",
}

// fixedDate 把签名钉到向量给定的 X-Sdk-Date：注入头集合即可（sign 函数对
// 已有 X-Sdk-Date 不覆盖）。
func fixedDate(v string) [][2]string {
	return [][2]string{{"X-Sdk-Date", v}}
}

func headerValue(hs [][2]string, name string) string {
	for _, kv := range hs {
		if strings.EqualFold(kv[0], name) {
			return kv[1]
		}
	}
	return ""
}

func TestSignOfficeAceSDKVector(t *testing.T) {
	signed, err := signOfficeAce("SDK-HMAC-SHA256", "GET",
		"https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v1/subscription",
		[][2]string{{"x-subscription-type", "v2"}, {"X-Sdk-Date", "20261010T031500Z"}},
		nil, vectorCred, "", false)
	if err != nil {
		t.Fatal(err)
	}
	wantCReq := "GET\n/v1/subscription/\n\n" +
		"host:modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com\n" +
		"x-project-id:pid-00000000000000000000000000000001\n" +
		"x-sdk-date:20261010T031500Z\n" +
		"x-security-token:STTESTSECURITYTOKEN0001\n" +
		"x-subscription-type:v2\n\n" +
		"host;x-project-id;x-sdk-date;x-security-token;x-subscription-type\n" +
		emptyBodySHA256
	if signed.CanonicalReq != wantCReq {
		t.Errorf("canonical request mismatch:\n got: %q\nwant: %q", signed.CanonicalReq, wantCReq)
	}
	if signed.StringToSign != "SDK-HMAC-SHA256\n20261010T031500Z\ne934f58149c2ff2121b72fff77e515341053c64220279158ed76e10fad7646a3" {
		t.Errorf("string to sign mismatch: %q", signed.StringToSign)
	}
	wantAuthz := "SDK-HMAC-SHA256 Access=AKTESTACCESSKEY0001, SignedHeaders=host;x-project-id;x-sdk-date;x-security-token;x-subscription-type, Signature=dda3e12a3463ccf699bce0fd863fb678c933aa377a79c8f5233026c630a43494"
	if got := headerValue(signed.Headers, "Authorization"); got != wantAuthz {
		t.Errorf("authorization mismatch:\n got: %s\nwant: %s", got, wantAuthz)
	}
}

func TestSignV11Vector(t *testing.T) {
	signed, err := SignV11("GET",
		"https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v1/subscription",
		[][2]string{{"x-subscription-type", "v2"}, {"X-Sdk-Date", "20261010T031500Z"}},
		nil, vectorCred, "cn-southwest-2")
	if err != nil {
		t.Fatal(err)
	}
	wantSt := "V11-HMAC-SHA256\n20261010T031500Z\n20261010/cn-southwest-2/apic\ne934f58149c2ff2121b72fff77e515341053c64220279158ed76e10fad7646a3"
	if signed.StringToSign != wantSt {
		t.Errorf("string to sign mismatch:\n got: %q\nwant: %q", signed.StringToSign, wantSt)
	}
	wantAuthz := "V11-HMAC-SHA256 Credential=AKTESTACCESSKEY0001/20261010/cn-southwest-2/apic, SignedHeaders=host;x-project-id;x-sdk-date;x-security-token;x-subscription-type, Signature=43806175f2f6b0d16c6698eea222bd39db3830caeee2d9b68b3ac21170270fba"
	if got := headerValue(signed.Headers, "Authorization"); got != wantAuthz {
		t.Errorf("authorization mismatch:\n got: %s\nwant: %s", got, wantAuthz)
	}
}

func TestSignV11PostBodyVector(t *testing.T) {
	signed, err := SignV11("POST",
		"https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v1/subscription/bonus/claim",
		[][2]string{{"Content-Type", "application/json"}, {"X-Sdk-Date", "20261010T031500Z"}},
		[]byte("{}"), vectorCred, "cn-southwest-2")
	if err != nil {
		t.Fatal(err)
	}
	// sha256("{}") = 44136fa3…
	wantCReq := "POST\n/v1/subscription/bonus/claim/\n\n" +
		"content-type:application/json\n" +
		"host:modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com\n" +
		"x-project-id:pid-00000000000000000000000000000001\n" +
		"x-sdk-date:20261010T031500Z\n" +
		"x-security-token:STTESTSECURITYTOKEN0001\n\n" +
		"content-type;host;x-project-id;x-sdk-date;x-security-token\n" +
		"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if signed.CanonicalReq != wantCReq {
		t.Errorf("canonical request mismatch:\n got: %q\nwant: %q", signed.CanonicalReq, wantCReq)
	}
	if got := headerValue(signed.Headers, "Authorization"); got != "V11-HMAC-SHA256 Credential=AKTESTACCESSKEY0001/20261010/cn-southwest-2/apic, SignedHeaders=content-type;host;x-project-id;x-sdk-date;x-security-token, Signature=72bc4b415e3fe720b0bb040d0d4c78a18eb4821babc814381d8336d27c31d96a" {
		t.Errorf("authorization mismatch: %s", got)
	}
}

func TestSignV11QueryVector(t *testing.T) {
	signed, err := SignV11("GET",
		"https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v1/subscription/usage?sku_attr_code=officeace_points&aggregation=day",
		[][2]string{{"X-Sdk-Date", "20261010T031500Z"}},
		nil, vectorCred, "cn-southwest-2")
	if err != nil {
		t.Fatal(err)
	}
	// WHATWG 排序：aggregation 在 sku_attr_code 前。
	if !strings.Contains(signed.CanonicalReq, "\naggregation=day&sku_attr_code=officeace_points\n") {
		t.Errorf("query canonicalization mismatch: %q", signed.CanonicalReq)
	}
	if got := headerValue(signed.Headers, "Authorization"); got != "V11-HMAC-SHA256 Credential=AKTESTACCESSKEY0001/20261010/cn-southwest-2/apic, SignedHeaders=host;x-project-id;x-sdk-date;x-security-token, Signature=51edfd940e5c2399ec4c7cac17eb7dda00b8f2a1e232bfb3e5f3985f42a4d9ac" {
		t.Errorf("authorization mismatch: %s", got)
	}
}

func TestSignV11EscapingVector(t *testing.T) {
	// strict 集：!'()* 必须转义（与 CodeArts 的 JS 集不同处）。
	signed, err := SignV11("GET",
		"https://gw.example.com/v1/a(b)c!d/e'f/g*h/i~j/things?q=a b&r=x'y&s=1*2&t=%7Bbrace%7D",
		[][2]string{{"X-Sdk-Date", "20261010T031500Z"}},
		nil, vectorCred, "cn-southwest-2")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/v1/a%28b%29c%21d/e%27f/g%2Ah/i~j/things/"
	wantQuery := "q=a%20b&r=x%27y&s=1%2A2&t=%7Bbrace%7D"
	if !strings.Contains(signed.CanonicalReq, "\n"+wantPath+"\n"+wantQuery+"\n") {
		t.Errorf("path/query escaping mismatch:\n%s", signed.CanonicalReq)
	}
	if got := headerValue(signed.Headers, "Authorization"); got != "V11-HMAC-SHA256 Credential=AKTESTACCESSKEY0001/20261010/cn-southwest-2/apic, SignedHeaders=host;x-project-id;x-sdk-date;x-security-token, Signature=7e9cf7348c3a1e9c84167fb8ef074a939e309553b724d7ecfe4034fe6ca59074" {
		t.Errorf("authorization mismatch: %s", got)
	}
}

// CodeArts SDK 的行为钉子（无金向量文件，按 agent2api codearts/signer.rs 规则钉）：
// JS 未保留集（!'()* 字面量）、GET 恒签空体、X-Domain-Id 注入。
func TestSignSDKCodeArtsKeepSet(t *testing.T) {
	cred := SigningCredential{AccessKeyID: "AK", SecretKey: "SK",
		SecurityToken: "ST", DomainID: "DOM"}
	signed, err := SignSDK("GET",
		"https://snap-access.cn-north-4.myhuaweicloud.com/v1/model/builtin",
		[][2]string{{"X-Sdk-Date", "20261010T031500Z"}, {"plugin-name", "snap_vscode"}},
		nil, cred, false)
	if err != nil {
		t.Fatal(err)
	}
	// include_host=false ⇒ 无 Host 头；domain 注入 X-Domain-Id。
	if strings.Contains(signed.CanonicalReq, "host:") {
		t.Errorf("include_host=false 时不应有 Host: %q", signed.CanonicalReq)
	}
	if !strings.Contains(signed.CanonicalReq, "x-domain-id:DOM\n") {
		t.Errorf("缺少 X-Domain-Id: %q", signed.CanonicalReq)
	}
	if !strings.HasSuffix(signed.CanonicalReq, emptyBodySHA256) {
		t.Errorf("GET 应签空体摘要: %q", signed.CanonicalReq)
	}
	// 路径尾斜杠。
	if !strings.Contains(signed.CanonicalReq, "/v1/model/builtin/\n") {
		t.Errorf("canonicalURI 应补尾斜杠: %q", signed.CanonicalReq)
	}
}

func TestSignSDKCodeArtsEscaping(t *testing.T) {
	cred := SigningCredential{AccessKeyID: "AK", SecretKey: "SK"}
	signed, err := SignSDK("GET", "https://gw.example.com/v1/a(b)c!d/e'f/g*h/i~j/things",
		[][2]string{{"X-Sdk-Date", "20261010T031500Z"}}, nil, cred, false)
	if err != nil {
		t.Fatal(err)
	}
	// JS 集：!'()* 保持字面量（~ 同样）。
	if !strings.Contains(signed.CanonicalReq, "/v1/a(b)c!d/e'f/g*h/i~j/things/\n") {
		t.Errorf("JS 未保留集应保留 !'()*~: %q", signed.CanonicalReq)
	}
}

func TestSignSDKPostSignsBody(t *testing.T) {
	cred := SigningCredential{AccessKeyID: "AK", SecretKey: "SK"}
	signed, err := SignSDK("POST", "https://x.example.com/api/v2/chat/completions",
		[][2]string{{"X-Sdk-Date", "20261010T031500Z"}}, []byte("{}"), cred, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(signed.CanonicalReq, "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a") {
		t.Errorf("POST 应签真实体摘要: %q", signed.CanonicalReq)
	}
}

// ---------------------------------------------------------------------------
// DPoP
// ---------------------------------------------------------------------------

// d1Key d=1 的固定 P-256 私钥（公钥=生成元，agent2api dpop.rs 测试同款）。
func d1Key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key := new(ecdsa.PrivateKey)
	key.D = big.NewInt(1)
	key.Curve = elliptic.P256()
	key.PublicKey.Curve = key.Curve
	key.PublicKey.X, key.PublicKey.Y = key.Curve.ScalarBaseMult(key.D.Bytes())
	return key
}

func TestDPoPProofShape(t *testing.T) {
	key := d1Key(t)
	proof, err := DPoPProof(key, "POST",
		"https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens",
		time.Unix(1790439420, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS 应三段式: %s", proof)
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(hb, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" || header["typ"] != "dpop+jwt" {
		t.Errorf("header mismatch: %v", header)
	}
	jwk := header["jwk"].(map[string]any)
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		t.Errorf("jwk mismatch: %v", jwk)
	}
	if _, has := jwk["d"]; has {
		t.Error("proof 头绝不能带私钥 d")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(pb, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["htm"] != "POST" || payload["iat"].(float64) != 1790439420 {
		t.Errorf("payload mismatch: %v", payload)
	}
	if len(payload["jti"].(string)) != 64 {
		t.Error("jti 应为 32 字节 hex（64 字符）")
	}
	// 签名段 64 字节。
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Errorf("签名应 64 字节 r||s: %v", err)
	}
	// 用嵌入公钥验签（本模块的真正验收）。
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Error("proof 必须能用它自己带的公钥验过")
	}
	// 小写方法规范化成大写。
	lower, _ := DPoPProof(key, "post", "https://x.example.com/a", time.Unix(1, 0))
	pb2, _ := base64.RawURLEncoding.DecodeString(strings.Split(lower, ".")[1])
	var p2 map[string]any
	_ = json.Unmarshal(pb2, &p2)
	if p2["htm"] != "POST" {
		t.Error("htm 应被规范化为大写")
	}
}

func TestKeyPairRoundtrip(t *testing.T) {
	pair, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := LoadKeyPair(pair)
	if err != nil {
		t.Fatalf("落盘形态应能恢复: %v", err)
	}
	wantX, err := base64.RawURLEncoding.DecodeString(pair.PublicKeyJwk.X)
	if err != nil {
		t.Fatal(err)
	}
	if key.PublicKey.X.Cmp(new(big.Int).SetBytes(wantX)) != 0 {
		t.Error("恢复的公钥与落盘公钥不一致")
	}
	// 公钥被改坏要报错。
	bad := pair
	bad.PublicKeyJwk.X = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err := LoadKeyPair(bad); err == nil {
		t.Error("公私钥不一致应报错")
	}
	// 曲线不对要报错。
	wrong := pair
	wrong.PrivateKeyJwk.Crv = "P-384"
	if _, err := LoadKeyPair(wrong); err == nil {
		t.Error("曲线不对应报错")
	}
}
