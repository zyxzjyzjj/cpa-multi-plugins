package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestLifecycleRegistrationReturnsEnvelope(t *testing.T) {
	yamlDoc := "enabled: true\npriority: 1\nbase_url: \"https://snap-access.cn-north-4.myhuaweicloud.com\"\napi_mode: \"agent\"\nlanguage: \"zh-cn\"\n"
	req, _ := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte(yamlDoc)),
		"schema_version": pluginabi.SchemaVersion,
	})

	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		raw, err := handleMethod(method, req)
		if err != nil {
			t.Fatalf("%s: handleMethod error: %v", method, err)
		}
		if len(raw) == 0 {
			t.Fatalf("%s: empty envelope", method)
		}
		var env struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s: decode envelope: %v", method, err)
		}
		if !env.OK {
			t.Fatalf("%s: envelope not ok: %s", method, raw)
		}
		if len(env.Result) == 0 {
			t.Fatalf("%s: empty result", method)
		}
		cfg := config()
		if cfg.BaseURL != "https://snap-access.cn-north-4.myhuaweicloud.com" || cfg.Language != "zh-cn" {
			t.Fatalf("%s: config not applied: %+v", method, cfg)
		}
		t.Logf("%s -> %d bytes, base_url=%s language=%s api_mode=%s", method, len(raw), cfg.BaseURL, cfg.Language, cfg.APIMode)
	}
}
