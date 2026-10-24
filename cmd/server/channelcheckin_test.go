package main

import (
	"encoding/json"
	"testing"
)

// TestChannelCheckinDefaultEnabledOnOldConfig 老 config（无 channel_checkin 键）
// 必须按「启用」处理——新功能默认开，升级用户零配置即生效。
// parseConfigBytes 测试用：从 JSON 字节解析配置（生产路径是 Load，见 config.go:504）。
func parseConfigBytes(raw []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func TestChannelCheckinDefaultEnabledOnOldConfig(t *testing.T) {
	c, err := parseConfigBytes([]byte(`{"schedule":{"checkin_hours":[9,21],"checkin_enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.channelCheckinEnabled() {
		t.Error("老配置缺 channel_checkin 键时应默认启用")
	}
	// 显式 false 生效。
	off := false
	c.Schedule.ChannelCheckin.Enabled = &off
	if c.channelCheckinEnabled() {
		t.Error("显式 false 应关闭")
	}
	on := true
	c.Schedule.ChannelCheckin.Enabled = &on
	if !c.channelCheckinEnabled() {
		t.Error("显式 true 应启用")
	}
}

// TestChannelCheckinExclude 排除名单（大小写不敏感、去空白）。
func TestChannelCheckinExclude(t *testing.T) {
	raw := []byte(`{"schedule":{"channel_checkin":{"enabled":true,"exclude":[" CodeArts ","AUTOCLAW"]}}}`)
	c, err := parseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !c.channelCheckinExcluded("codearts") || !c.channelCheckinExcluded("CODEARTS") {
		t.Error("codearts 应在排除名单里（大小写不敏感）")
	}
	if !c.channelCheckinExcluded("autoclaw") {
		t.Error("autoclaw 应在排除名单里")
	}
	if c.channelCheckinExcluded("trae") || c.channelCheckinExcluded("officeace") || c.channelCheckinExcluded("qoderwork") {
		t.Error("未列出的渠道不应被排除")
	}
}

// TestChannelCheckinRoundTripJSON channel_checkin 段能正确序列化/反序列化。
func TestChannelCheckinRoundTripJSON(t *testing.T) {
	c := Default()
	if !c.channelCheckinEnabled() {
		t.Error("Default 应启用渠道签到")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := parseConfigBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.channelCheckinEnabled() {
		t.Error("序列化往返后仍应启用")
	}
}
