package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBenefitBootstrapPersistsAndSurvivesCredentialRotation(t *testing.T) {
	resetModelCache(t)
	var calls atomic.Int32
	fixture := modelFixture(t, "gateway-config")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/benefit-gateway-config" {
			fmt.Fprint(w, `{"enabled":true}`)
			return
		}
		if r.URL.Path != "/api/v1/gateway/config" || !strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=host;x-sdk-date;x-security-token,") {
			t.Error("incorrect gateway discovery")
			http.Error(w, "bad", 400)
			return
		}
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.BaseURL = server.URL
	cfg.BenefitGatewayURL = server.URL
	cred := &credential{AccessKeyID: "fake-ak", SecretAccessKey: "fake-sk", SecurityToken: "fake-sts", DomainID: "test-domain", UserID: "test-user"}
	if warning := bootstrapBenefitCatalog(cfg, cred); warning != "" {
		t.Fatal(warning)
	}
	if calls.Load() != 2 {
		t.Fatal("initial discovery not performed")
	}
	path, _ := benefitStatePath(cfg, cred)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fake-sts") || strings.Contains(string(b), "fake-sk") {
		t.Fatal("secrets in catalog")
	}
	benefitCatalogMemory.Lock()
	benefitCatalogMemory.entries = map[string]benefitCatalogState{}
	benefitCatalogMemory.Unlock()
	cred.AccessKeyID = "rotated-ak"
	cred.SecurityToken = "rotated-sts"
	cfg.Schedule.Enabled = false
	if warning := bootstrapBenefitCatalog(cfg, cred); warning != "" || calls.Load() != 2 {
		t.Fatal("rotation/restart/switch lost saved catalogue")
	}
	if state, ok := storedBenefitCatalog(cfg, cred); !ok || len(state.Models) != 3 {
		t.Fatal("saved models unavailable")
	}
	other := *cred
	other.UserID = "other-user"
	if _, ok := storedBenefitCatalog(cfg, &other); ok {
		t.Fatal("catalogue leaked across accounts")
	}
}

func TestAuxiliaryDiscoveryCancellationStopsNetwork(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(cancelled) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := auxiliaryHTTPDirect(ctx, defaultConfig(), "GET", server.URL, nil, nil); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("discovery failed to cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream connection survived cancellation")
	}
}

func TestCreditMetricsAreNotTokenMetrics(t *testing.T) {
	s, err := parseQuotaSnapshot([]byte(`{"metrics":[{"name":"usageBonusPackageCredit","show":true,"package_credit_amount":1000,"package_credit_used":10,"package_credit_remain":990}]}`))
	if err != nil || len(s.Meters) != 1 || s.Meters[0].CreditRemaining == nil || *s.Meters[0].CreditRemaining != 990 || s.Meters[0].AllowanceTokens != 0 {
		t.Fatalf("credit balance lost: %+v %v", s, err)
	}
}
