package cosy

// Profile 描述一个 COSY 协议变体的全部静态参数。
//
// Qoder / QoderWork / 千问办公三套上游共用同一套签名算法，仅常量不同。
// 差异点集中在 HTTP 头取值与 payload 字段顺序上：
//   - PayloadOrder：签名覆盖 base64(payload) 的字节序列，
//     qoderwork 侧按 key 排序序列化，Qoder(API-Console) 侧按结构体声明序序列化，
//     两者不兼容，选错会导致全部请求 403 Signature invalid。
type Profile struct {
	Name string

	// 端点
	GatewayBase  string // 推理网关，例 https://gateway.qoder.com.cn
	OpenAPIBase  string // 开放接口，例 https://openapi.qoder.com.cn
	OAuthBase    string // OAuth 设备流，例 https://qoder.com
	ClientID     string
	ClientSecret string

	// 会话签名相关头
	ClientType      string // Cosy-ClientType
	CosyVersion     string // Cosy-Version（缺省可通过 WithCosyVersion 覆盖）
	DataPolicy      string // Cosy-Data-Policy
	BusinessProduct string // Cosy-Business-Product
	BusinessType    string // Cosy-Business-Type
	Scene           string // Cosy-Scene
	MachineType     string // Cosy-MachineType
	MachineOS       string // Cosy-MachineOS
	LoginVersion    string // login-version
	ClientIP        string // cosy-clientip

	// 业务参数
	SessionType    string // 请求体 session_type
	SubTask        string // 请求体 sub_task
	AliyunUserType string // identity.user_type
	UserAgent      string // 对话请求 User-Agent
	ControlUA      string // 控制面请求 User-Agent（缺省回落 UserAgent）
	PayloadOrder   PayloadOrder

	// 额外静态头（key/value 成对）
	ExtraHeaders [][2]string

	// 是否发送压缩相关头
	AcceptEncoding string

	// TraceContext 是否随请求发送 Traceparent（由 requestId 派生）。
	TraceContext bool
}

// WithCosyVersion 返回替换 Cosy-Version 后的副本。
func (p Profile) WithCosyVersion(v string) Profile {
	if v != "" {
		p.CosyVersion = v
	}
	return p
}

// MachineTypeOr 返回 Cosy-MachineType：Profile 显式指定则用它，
// 否则回落到账号机器指纹（QoderWork 侧为 uuid 去横线前 18 位）。
func (p Profile) MachineTypeOr(fallback string) string {
	if p.MachineType != "" {
		return p.MachineType
	}
	return fallback
}

// ControlUserAgent 返回控制面请求使用的 UA。
func (p Profile) ControlUserAgent() string {
	if p.ControlUA != "" {
		return p.ControlUA
	}
	return p.UserAgent
}

// HeaderOrDefault 返回头值，空则回落 def。
func HeaderOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ProfileQoderWork 返回 QoderWork(qoder.com.cn) 侧的变体。
// 来源：thirdparty/qoderwork/qoderwork2api-main/internal/upstream/cosy.go
func ProfileQoderWork() Profile {
	return Profile{
		Name:            "qoderwork",
		GatewayBase:     "https://gateway.qoder.com.cn",
		OpenAPIBase:     "https://openapi.qoder.com.cn",
		ClientType:      "5",
		CosyVersion:     "0.1.43",
		DataPolicy:      "AGREE",
		BusinessProduct: "qoder_work",
		MachineType:     "", // 运行时取 Fingerprint.MachineType（uuid 去横线前 18 位）
		LoginVersion:    "v2",
		ClientIP:        "169.254.198.161",
		SessionType:     "qodercli",
		AliyunUserType:  "personal_professional_trial",
		UserAgent:       "Go-http-client/2.0",
		PayloadOrder:    OrderSorted,
		AcceptEncoding:  "identity",
	}
}

// ProfileQoder 返回 Qoder(qoder.sh) 侧的变体。
// 来源：thirdparty/qoder/API-Console-main/internal/qoder/{runtime,request}.go
func ProfileQoder() Profile {
	return Profile{
		Name:            "qoder",
		GatewayBase:     "https://api2.qoder.sh",
		OpenAPIBase:     "https://openapi.qoder.sh",
		OAuthBase:       "https://qoder.com",
		ClientID:        "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		ClientType:      "6",
		CosyVersion:     "1.0.45",
		DataPolicy:      "agree",
		BusinessProduct: "qoder_work",
		BusinessType:    "agent",
		Scene:           "qwork",
		MachineType:     "5",
		MachineOS:       "x86_64_win32",
		LoginVersion:    "v2",
		SessionType:     "qoder_work",
		SubTask:         "ws_builtin_general",
		UserAgent:       "node",
		ControlUA:       "qoder/1.0.45",
		PayloadOrder:    OrderStruct,
		TraceContext:    true,
		ExtraHeaders: [][2]string{
			{"Accept-Language", "*"},
			{"Connection", "keep-alive"},
			{"Sec-Fetch-Mode", "cors"},
		},
	}
}

// ProfileQwenWork 返回千问办公(gateway.qwenwork.cn) 侧的变体。
// 来源：本地千问办公客户端（qoderclicn 1.1.59）抓包实测：
// product=cli / clienttype=5 / scene=assistant / UA=undici / clientip=machineid，
// chat 请求另带 cosy-machinehostname；运行时字段随凭证持久复用。
func ProfileQwenWork() Profile {
	return Profile{
		Name:            "qwenwork",
		GatewayBase:     "https://gateway.qwenwork.cn",
		OpenAPIBase:     "https://openapi.qoder.com.cn", // CN CLI 复用 qoder CN 开放接口（deviceToken/refresh）
		ClientType:      "5",
		CosyVersion:     "1.1.59",
		DataPolicy:      "disagree",
		BusinessProduct: "cli",
		BusinessType:    "agent",
		Scene:           "assistant",
		MachineType:     "5",
		MachineOS:       "x86_64_win32",
		LoginVersion:    "v2",
		ClientIP:        "@machine",
		SessionType:     "qodercli",
		UserAgent:       "undici",
		PayloadOrder:    OrderStruct,
		AcceptEncoding:  "identity",
		TraceContext:    true,
		ExtraHeaders: [][2]string{
			{"Accept-Language", "*"},
			{"Sec-Fetch-Mode", "cors"},
		},
	}
}

// ChatPath COSY 流式对话端点（三家一致）。
const ChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// ModelsPath 模型列表端点（GET）。
const ModelsPath = "/algo/api/v2/model/list?Encode=1"
