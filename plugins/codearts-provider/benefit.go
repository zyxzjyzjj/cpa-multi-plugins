package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The subscription statistics document reports the paid package meters only.
// The limited-time benefit pool is accounted separately by the benefit gateway,
// so a benefit model can fail with "insufficient quota" while every meter in
// the panel still reads zero. This file reads that second account so the panel
// can show both.
//
//	GET {benefit_gateway_url}/api/v1/user/tokens/balance
//	{"error_code":"0000","error_msg":"success","result":{
//	   "channel":"codearts","daily_token_limit":10000000,"daily_tokens_used":10231428,
//	   "monthly_token_limit":0,"monthly_tokens_used":10313740,"total_balance":0}}
const epBenefitBalance = "/api/v1/user/tokens/balance"

// benefitBalance is one account's limited-time benefit allowance. A zero limit
// means that dimension is uncapped, which is not the same as exhausted.
type benefitBalance struct {
	Channel           string `json:"channel"`
	DailyTokenLimit   int64  `json:"daily_token_limit"`
	DailyTokensUsed   int64  `json:"daily_tokens_used"`
	MonthlyTokenLimit int64  `json:"monthly_token_limit"`
	MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
	TotalBalance      int64  `json:"total_balance"`
}

// RemainingDaily is the tokens left in the daily pool, or -1 when the account
// has no daily cap.
func (b benefitBalance) RemainingDaily() int64 {
	if b.DailyTokenLimit <= 0 {
		return -1
	}
	if left := b.DailyTokenLimit - b.DailyTokensUsed; left > 0 {
		return left
	}
	return 0
}

// DailyPercent is the consumed share of the daily pool, or -1 when uncapped.
// Values above 100 are reported as-is: the gateway keeps counting usage after
// the cap is reached, and hiding that would understate the overrun.
func (b benefitBalance) DailyPercent() float64 {
	if b.DailyTokenLimit <= 0 {
		return -1
	}
	return float64(b.DailyTokensUsed) / float64(b.DailyTokenLimit) * 100
}

type benefitBalanceResponse struct {
	ErrorCode string          `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Result    *benefitBalance `json:"result"`
}

func parseBenefitBalance(body []byte) (benefitBalance, error) {
	var parsed benefitBalanceResponse
	if errUnmarshal := json.Unmarshal(body, &parsed); errUnmarshal != nil {
		return benefitBalance{}, fmt.Errorf("invalid benefit balance response: %w", errUnmarshal)
	}
	// The benefit gateway reports failure inside a successful HTTP response, so
	// the envelope code is the only reliable success signal.
	if parsed.ErrorCode != "0000" {
		return benefitBalance{}, fmt.Errorf("benefit balance returned %s: %s", parsed.ErrorCode, truncate(parsed.ErrorMsg, 120))
	}
	if parsed.Result == nil {
		return benefitBalance{}, fmt.Errorf("benefit balance response has no result")
	}
	return *parsed.Result, nil
}

// fetchBenefitBalance reads the daily benefit allowance for one credential. It
// is signed exactly like the claim request, which the same gateway accepts.
func fetchBenefitBalance(cfg *Config, cred *credential, callbackIDs ...string) (benefitBalance, error) {
	if cfg == nil || strings.TrimSpace(cfg.BenefitGatewayURL) == "" {
		return benefitBalance{}, fmt.Errorf("no benefit gateway URL is configured")
	}
	if cred == nil || !cred.valid() {
		return benefitBalance{}, fmt.Errorf("benefit balance requires an account credential")
	}
	// The ABI cannot set an independent deadline on host.http.do. Never launch
	// this optional request under a detached/background context. The management
	// caller owns cancellation (the dashboard aborts this request after 5s).
	if len(callbackIDs) == 0 || strings.TrimSpace(callbackIDs[0]) == "" {
		return benefitBalance{}, fmt.Errorf("benefit balance requires a cancellable management request")
	}
	endpoint := strings.TrimRight(cfg.BenefitGatewayURL, "/") + epBenefitBalance
	// Developer gateway is not the regional activity service. Match the official
	// host/date/STS signature without X-Domain-Id or extra regional headers.
	copyCred := *cred
	copyCred.DomainID = ""
	signed, errSign := signRequest(http.MethodGet, endpoint, nil, nil, &copyCred, true)
	if errSign != nil {
		return benefitBalance{}, fmt.Errorf("sign benefit balance request: %w", errSign)
	}
	response, errDo := hostHTTPDoContext(callbackIDs[0], http.MethodGet, endpoint, signed, nil)
	if errDo != nil {
		return benefitBalance{}, fmt.Errorf("benefit balance request failed or was cancelled")
	}
	if response.StatusCode != http.StatusOK {
		return benefitBalance{}, fmt.Errorf("benefit balance returned HTTP %d", response.StatusCode)
	}
	balance, errParse := parseBenefitBalance(response.Body)
	if errParse != nil {
		return benefitBalance{}, fmt.Errorf("%s", redactUpstreamError(errParse.Error(), cred, false))
	}
	return balance, nil
}
