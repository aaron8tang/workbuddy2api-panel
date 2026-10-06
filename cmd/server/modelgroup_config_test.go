package main

// 模型组配置校验测试：config.json 的 model_groups.groups 段走与启动同一套
// ParseConfig/normalize 校验（组名合法性/唯一性/保留字）。

import "testing"

func TestConfigModelGroupsValidation(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"absent key is fine", `{}`, false},
		{"empty groups is fine", `{"model_groups":{"groups":[]}}`, false},
		{"valid group", `{"model_groups":{"groups":[{"name":"dev","models":["cn:glm-5.2"],"accounts":["u1"]}]}}`, false},
		{"duplicate name", `{"model_groups":{"groups":[{"name":"dev"},{"name":"dev"}]}}`, true},
		{"reserved v1", `{"model_groups":{"groups":[{"name":"v1"}]}}`, true},
		{"uppercase", `{"model_groups":{"groups":[{"name":"Dev"}]}}`, true},
		{"bad char", `{"model_groups":{"groups":[{"name":"de v"}]}}`, true},
		{"empty name", `{"model_groups":{"groups":[{"name":""}]}}`, true},
	}
	for _, tc := range cases {
		_, err := ParseConfig([]byte(tc.raw))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}

	// 合法配置：trim 规范化保留在解析结果里（面板保存路径依赖同一行为）。
	c, err := ParseConfig([]byte(`{"model_groups":{"groups":[{"name":"dev","models":[" cn:glm-5.2 "]}]}}`))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if len(c.ModelGroups.Groups) != 1 || c.ModelGroups.Groups[0].Name != "dev" {
		t.Fatalf("unexpected groups: %+v", c.ModelGroups.Groups)
	}
	if len(c.ModelGroups.Groups[0].Models) != 1 || c.ModelGroups.Groups[0].Models[0] != "cn:glm-5.2" {
		t.Fatalf("models trim failed: %q", c.ModelGroups.Groups[0].Models)
	}
}
