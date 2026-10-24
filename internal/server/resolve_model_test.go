package server

import "testing"

func TestResolveModelPrefixes(t *testing.T) {
	cases := []struct {
		in    string
		realm string
		bare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-x", "global", "gpt-x"},
		{"trae:glm-5.2", "trae", "glm-5.2"},
		{"qoder:Qwen3.7-Max", "qoder", "Qwen3.7-Max"},
		{"qoderwork:qwen3.7-max", "qoderwork", "qwen3.7-max"},
		{"qwenwork:flash", "qwenwork", "flash"},
		// 组裁决 "模型|档位" 语法在 bare 中原样保留（由渠道网关拆分）
		{"qoderwork:qwen3.7-max|xhigh", "qoderwork", "qwen3.7-max|xhigh"},
		// 未知前缀 → 主链路裸名
		{"unknown:x", "cn", "unknown:x"},
		// 裸名
		{"glm-5.2", "cn", "glm-5.2"},
		// 只剥第一个冒号
		{"qoder:a:b", "qoder", "a:b"},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.realm || bare != c.bare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}
