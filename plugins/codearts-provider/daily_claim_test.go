package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type welfareFixture struct {
	mu              sync.Mutex
	status          string
	claimCalls      atomic.Int32
	confirmCalls    atomic.Int32
	failConfirm     bool
	badClaim        bool
	noConfirmChange bool
	keys            []string
}

func newWelfareFixture(t *testing.T) (*Config, pluginapi.HostAuthFileEntry, *credential, *welfareFixture) {
	t.Helper()
	cfg := defaultConfig()
	cfg.Schedule.Enabled = false
	file := pluginapi.HostAuthFileEntry{AuthIndex: "claim-fixture", Provider: providerID, Name: "fixture.json", Path: filepath.Join(t.TempDir(), "fixture.json")}
	cred := &credential{DomainID: "fixture-account", UserID: "fixture-user", AccessKeyID: "fake-ak", SecretAccessKey: "fake-sk", SecurityToken: "fake-token"}
	f := &welfareFixture{status: "ELIGIBLE"}
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{file}})
		case "host.auth.get":
			doc, _ := buildAuthFileDocument(*cred)
			return json.Marshal(map[string]any{"json": json.RawMessage(doc)})
		case "host.http.do":
			f.mu.Lock()
			defer f.mu.Unlock()
			req := request.(map[string]any)
			endpoint := req["url"].(string)
			headers := http.Header(req["headers"].(map[string][]string))
			if strings.HasSuffix(endpoint, "/statistics/plugin") {
				return json.Marshal(modelJSON([]byte(`{"metrics":[]}`)))
			}
			if !strings.HasPrefix(endpoint, cfg.BaseURL+"/v1/ops/") || headers.Get("Agent-Type") != "PromptCenter" || headers.Get("X-Domain-Id") != cred.DomainID {
				t.Error("wrong welfare endpoint or regional signing headers")
			}
			b := req["body"].([]byte)
			var data map[string]any
			if len(b) > 0 {
				_ = json.Unmarshal(b, &data)
			}
			switch endpoint {
			case cfg.BaseURL + epWelfareDelivery:
				if req["method"] != "GET" || len(b) != 0 {
					t.Error("invalid delivery request")
				}
				body := fmt.Sprintf(`{"code":0,"data":{"items":[{"campaignId":1,"type":"USER_LOGIN","benefitAmount":1000,"benefitUnit":"CREDIT","claimable":%t,"status":%q},{"campaignId":4,"type":"NEW_USER_REGISTER","benefitUnit":"CREDIT","claimable":true}]}}`, f.status == "ELIGIBLE", f.status)
				return json.Marshal(modelJSON([]byte(body)))
			case cfg.BaseURL + epWelfareClaim:
				f.claimCalls.Add(1)
				if req["method"] != "POST" || data["campaignId"] != float64(1) || data["channel"] != "IDE" || data["idempotentKey"] == "" {
					t.Error("invalid activity claim request")
				}
				f.keys = append(f.keys, fmt.Sprint(data["idempotentKey"]))
				if f.badClaim {
					return json.Marshal(modelJSON([]byte(`{"error_code":"0000","result":{"channel":"codearts"}}`)))
				}
				f.status = "CLAIMED"
				return json.Marshal(modelJSON([]byte(`{"code":0,"data":{"campaignId":1,"status":"CLAIMED"}}`)))
			case cfg.BaseURL + epWelfareConfirm:
				f.confirmCalls.Add(1)
				if req["method"] != "POST" || data["campaignId"] != float64(1) || len(data) != 1 {
					t.Error("invalid confirmation body")
				}
				if f.failConfirm {
					return json.Marshal(modelJSON([]byte(`{"code":999,"message":"fake-token"}`)))
				}
				if !f.noConfirmChange {
					f.status = "CONFIRMED"
				}
				return json.Marshal(modelJSON([]byte(`{"code":0,"data":{"campaignId":1}}`)))
			}
			return nil, fmt.Errorf("unexpected endpoint")
		}
		return json.RawMessage(`{}`), nil
	})
	previous := config()
	currentConfig.Store(cfg)
	t.Cleanup(func() { stopScheduleSettings(); currentConfig.Store(previous) })
	return cfg, file, cred, f
}

func dailyClaimFixture(t *testing.T) (*Config, pluginapi.HostAuthFileEntry, *credential, *atomic.Int32) {
	cfg, file, cred, f := newWelfareFixture(t)
	return cfg, file, cred, &f.claimCalls
}

func TestDailyWelfareOfficialClaimConfirmAndScope(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	if cfg.DailyClaim.Enabled || len(cfg.visibleScheduleTasks()) != 3 {
		t.Fatal("daily task must be opt-in and visible")
	}
	if err := runDailyClaimSweep(dailyClaimTask(false), time.Now, false); err != nil || f.claimCalls.Load() != 0 {
		t.Fatal("disabled task ran")
	}
	if err := runDailyClaimSweep(dailyClaimTask(false), time.Now, true); err != nil {
		t.Fatal(err)
	}
	if f.claimCalls.Load() != 1 || f.confirmCalls.Load() != 1 {
		t.Fatal("did not execute claim AND confirmation")
	}
	p, _ := dailyClaimPath(cfg, file, cred)
	var s dailyClaimState
	if readPluginState(p, &s) != nil || s.Version != 2 || !s.Accepted {
		t.Fatal("verified result not saved")
	}
	b, _ := os.ReadFile(p)
	for _, secret := range []string{cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken, cred.UserID} {
		if strings.Contains(string(b), secret) {
			t.Fatal("state leaked a credential")
		}
	}
}

func TestDailyWelfareFailedConfirmationResumesWithoutClaimingTwice(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	f.failConfirm = true
	now := time.Now()
	if _, err := claimAccount(cfg, file, cred, now, true); err == nil || strings.Contains(err.Error(), "fake-token") {
		t.Fatal("failure hidden or secret leaked")
	}
	p, _ := dailyClaimPath(cfg, file, cred)
	var s dailyClaimState
	_ = readPluginState(p, &s)
	if s.Accepted || !s.Campaigns["1"].Claimed {
		t.Fatal("claim stage lost")
	}
	dailyClaimLocks.Delete(p)
	f.failConfirm = false
	if result, err := claimAccount(cfg, file, cred, now.Add(11*time.Minute), true); err != nil || result != "confirmed" {
		t.Fatalf("resume: %s %v", result, err)
	}
	if f.claimCalls.Load() != 1 || f.confirmCalls.Load() != 2 {
		t.Fatal("retry repeated claim instead of confirmation")
	}
}

func TestDailyWelfareNoFalseSuccessAndStableIdempotency(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	f.badClaim = true
	now := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := claimAccount(cfg, file, cred, now.Add(time.Duration(i)*11*time.Minute), true); err == nil {
			t.Fatal("old gateway envelope counted as success")
		}
	}
	if len(f.keys) != 2 || f.keys[0] != f.keys[1] {
		t.Fatal("retry changed persisted idempotency key")
	}
}

func TestDailyWelfareConfirmationMustAppearInDelivery(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	f.noConfirmChange = true
	if _, err := claimAccount(cfg, file, cred, time.Now(), true); err == nil {
		t.Fatal("unverified confirmation was reported successful")
	}
	p, _ := dailyClaimPath(cfg, file, cred)
	var s dailyClaimState
	_ = readPluginState(p, &s)
	if s.Accepted {
		t.Fatal("unverified result persisted")
	}
}

func TestDailyWelfareConcurrentDedupAndManualRechecksUpstream(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if _, err := claimAccount(cfg, file, cred, now, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.claimCalls.Load() != 1 {
		t.Fatal("concurrent duplicate claim")
	}
	f.status = "ELIGIBLE" // next day's login reward is available again.
	if _, err := claimAccount(cfg, file, cred, now.Add(24*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if f.claimCalls.Load() != 2 {
		t.Fatal("next day was suppressed")
	}
}

func TestDailyWelfareStorageAndRetryLimits(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	f.badClaim = true
	now := time.Now()
	for i := 0; i < 7; i++ {
		_, _ = claimAccount(cfg, file, cred, now.Add(time.Duration(i)*11*time.Minute), true)
	}
	if f.claimCalls.Load() != 6 {
		t.Fatal("daily retry cap violated")
	}
	p, _ := dailyClaimPath(cfg, file, cred)
	if err := os.WriteFile(p, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAccount(cfg, file, cred, now, true); err == nil || f.claimCalls.Load() != 6 {
		t.Fatal("corrupt state must fail closed")
	}
}

func TestDailyWelfarePreviouslyConfirmedStatusDoesNotSuppressNewEligibility(t *testing.T) {
	cfg, file, cred, f := newWelfareFixture(t)
	f.status = "CONFIRMED"
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, dailyClaimZone)
	if result, err := claimAccount(cfg, file, cred, now, false); err != nil || result != "already" {
		t.Fatal("could not read existing confirmation")
	}
	f.status = "ELIGIBLE"
	if result, err := claimAccount(cfg, file, cred, now.Add(11*time.Minute), false); err != nil || result != "confirmed" || f.claimCalls.Load() != 1 {
		t.Fatal("old confirmation suppressed newly delivered daily reward")
	}
}
