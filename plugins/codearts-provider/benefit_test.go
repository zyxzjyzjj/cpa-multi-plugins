package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Shapes captured live from the benefit gateway on 2026-09-21.

func TestParseBenefitBalanceMatchesUpstreamShape(t *testing.T) {
	body := []byte(`{"error_code":"0000","error_msg":"success","result":{"channel":"codearts",` +
		`"create_time":1789832970924,"daily_token_limit":10000000,"daily_tokens_used":10231428,` +
		`"domain_id":"9e5d1997c2e74fd0a06577be357d3d4c","expire_time":0,"monthly_token_limit":0,` +
		`"monthly_tokens_used":10313740,"total_balance":0}}`)

	balance, errParse := parseBenefitBalance(body)
	if errParse != nil {
		t.Fatalf("parseBenefitBalance: %v", errParse)
	}
	if balance.Channel != "codearts" {
		t.Errorf("channel = %q", balance.Channel)
	}
	if balance.DailyTokenLimit != 10000000 || balance.DailyTokensUsed != 10231428 {
		t.Errorf("daily = %d/%d, want 10231428/10000000", balance.DailyTokensUsed, balance.DailyTokenLimit)
	}
	if balance.MonthlyTokensUsed != 10313740 || balance.MonthlyTokenLimit != 0 {
		t.Errorf("monthly = %d/%d", balance.MonthlyTokensUsed, balance.MonthlyTokenLimit)
	}
}

func TestParseBenefitBalanceRejectsUnsuccessfulEnvelope(t *testing.T) {
	// The gateway answers with HTTP 200 and reports failure in the envelope, so a
	// non-"0000" code must never be treated as an allowance.
	for _, body := range []string{
		`{"error_code":"9999","error_msg":"not entitled","result":{"daily_token_limit":10}}`,
		`{"error_code":"0000"}`,
		`{"error_code":"0000","result":null}`,
		`not-json`,
	} {
		balance, errParse := parseBenefitBalance([]byte(body))
		if errParse == nil {
			t.Fatalf("unsuccessful response advertised an allowance: %s -> %+v", body, balance)
		}
		if balance.DailyTokenLimit != 0 || balance.DailyTokensUsed != 0 {
			t.Fatalf("rejected response leaked values: %+v", balance)
		}
	}
}

func TestBenefitDailyRemainingAndPercent(t *testing.T) {
	cases := []struct {
		name        string
		balance     benefitBalance
		wantRemain  int64
		wantPercent float64
	}{
		{"fresh", benefitBalance{DailyTokenLimit: 10000000, DailyTokensUsed: 2500000}, 7500000, 25},
		{"exhausted", benefitBalance{DailyTokenLimit: 10000000, DailyTokensUsed: 10000000}, 0, 100},
		// The gateway keeps counting past the cap, and the overrun is reported as
		// measured rather than clamped to 100%.
		{"over cap", benefitBalance{DailyTokenLimit: 10000000, DailyTokensUsed: 10231428}, 0, 102.31428},
		{"uncapped", benefitBalance{DailyTokenLimit: 0, DailyTokensUsed: 42000000}, -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.balance.RemainingDaily(); got != tc.wantRemain {
				t.Errorf("RemainingDaily = %d, want %d", got, tc.wantRemain)
			}
			if got := tc.balance.DailyPercent(); got != tc.wantPercent {
				t.Errorf("DailyPercent = %v, want %v", got, tc.wantPercent)
			}
		})
	}
}

func TestToQuotaFetchResponseReportsBenefitPool(t *testing.T) {
	snapshot := quotaSnapshot{
		Plan: "codearts.agent.trial", PlanName: "Free",
		ResetDate: "2026-10-03",
		Benefit:   &benefitBalance{Channel: "codearts", DailyTokenLimit: 10000000, DailyTokensUsed: 10231428},
	}
	response := toQuotaFetchResponse(snapshot)
	var found bool
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Window != "1d" {
				continue
			}
			found = true
			if bucket.RemainingFraction != 0 {
				t.Errorf("remaining = %v, want 0 for an over-cap pool", bucket.RemainingFraction)
			}
			if !strings.Contains(bucket.Description, "10231428") || !strings.Contains(bucket.Description, "10000000") {
				t.Errorf("description lost the raw counters: %q", bucket.Description)
			}
		}
	}
	if !found {
		t.Fatalf("no daily benefit bucket: %s", string(mustMarshal(t, response)))
	}
	var metrics int
	for _, metric := range response.Summary {
		if strings.HasPrefix(metric.Key, "benefit_") {
			metrics++
		}
	}
	if metrics != 2 {
		t.Errorf("benefit summary metrics = %d, want 2", metrics)
	}
}

func TestToQuotaFetchResponseOmitsUncappedAndMissingBenefit(t *testing.T) {
	for _, snapshot := range []quotaSnapshot{
		{Plan: "x"},
		{Plan: "x", Benefit: &benefitBalance{DailyTokenLimit: 0, DailyTokensUsed: 42000000}},
	} {
		encoded := string(mustMarshal(t, toQuotaFetchResponse(snapshot)))
		if strings.Contains(encoded, `"1d"`) || strings.Contains(encoded, "benefit_daily") {
			t.Fatalf("uncapped or absent benefit invented a quota: %s", encoded)
		}
	}
}

func TestFetchBenefitBalanceRefusesUselessInputs(t *testing.T) {
	cfg := defaultConfig()
	// No gateway configured.
	if _, err := fetchBenefitBalance(&Config{}, &credential{AccessKeyID: "ak", SecretAccessKey: "sk"}); err == nil {
		t.Fatal("an empty benefit gateway URL must not be requested")
	}
	// No credential: the allowance is per account and an unsigned request cannot
	// be attributed, so it must not be sent.
	if _, err := fetchBenefitBalance(cfg, &credential{}); err == nil {
		t.Fatal("an unsigned balance request must be refused")
	}
	if _, err := fetchBenefitBalance(nil, &credential{AccessKeyID: "ak", SecretAccessKey: "sk"}); err == nil {
		t.Fatal("missing configuration must fail closed")
	}
}

func TestAccountViewCarriesBenefit(t *testing.T) {
	encoded, errMarshal := json.Marshal(accountView{
		AuthIndex: "a1",
		Benefit:   &benefitBalance{DailyTokenLimit: 10000000, DailyTokensUsed: 123},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !strings.Contains(string(encoded), `"daily_tokens_used":123`) {
		t.Fatalf("panel would not see the counters: %s", encoded)
	}
}
