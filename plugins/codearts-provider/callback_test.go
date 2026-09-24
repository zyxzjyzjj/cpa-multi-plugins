package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManualCallbackSessionAndLoopback(t *testing.T) {
	const original = "http://127.0.0.1:43807/oauth/callback"
	for _, tc := range []struct {
		name, state, callback string
		want                  int
	}{
		{"numeric", "cpa-state", original + "?code=test-code&state=huawei-state", 200},
		{"localhost", "cpa-state", "http://localhost:43807/oauth/callback?code=test-code&state=huawei-state", 200},
		{"ipv6", "cpa-state", "http://[::1]:43807/oauth/callback?code=test-code&state=huawei-state", 200},
		{"wrong-state", "huawei-state", original + "?code=test-code&state=huawei-state", 400},
		{"wrong-port", "cpa-state", "http://localhost:43808/oauth/callback?code=test-code", 400},
		{"remote-host", "cpa-state", "http://example.com:43807/oauth/callback?code=test-code", 400},
		{"lookalike-host", "cpa-state", "http://localhost.example.com:43807/oauth/callback?code=test-code", 400},
		{"userinfo", "cpa-state", "http://user@localhost:43807/oauth/callback?code=test-code", 400},
		{"scheme", "cpa-state", "https://localhost:43807/oauth/callback?code=test-code", 400},
		{"path", "cpa-state", "http://localhost:43807/other?code=test-code", 400},
		{"no-code", "cpa-state", original + "?state=huawei-state", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &loginSession{state: "cpa-state", callbackURL: original, expires: time.Now().Add(time.Minute)}
			loginMu.Lock()
			previous := loginSessions
			loginSessions = map[string]*loginSession{s.state: s}
			loginMu.Unlock()
			t.Cleanup(func() { loginMu.Lock(); loginSessions = previous; loginMu.Unlock() })
			body, _ := json.Marshal(map[string]string{"state": tc.state, "callback_url": tc.callback})
			result := handleLoginCallback(pluginapi.ManagementRequest{Body: body})
			if result.StatusCode != tc.want {
				t.Fatalf("status=%d, want=%d body=%s", result.StatusCode, tc.want, result.Body)
			}
			if s.received != (tc.want == http.StatusOK) || s.callbackURL != original {
				t.Fatal("invalid callback accepted or original redirect URI was changed")
			}
			if tc.want == http.StatusOK {
				if s.authorizationCode != "test-code" {
					t.Fatal("authorization code was not retained")
				}
				if duplicate := handleLoginCallback(pluginapi.ManagementRequest{Body: body}); duplicate.StatusCode != http.StatusConflict {
					t.Fatal("duplicate callback was not rejected")
				}
			}
		})
	}
}
