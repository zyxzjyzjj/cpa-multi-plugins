package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	TaskDailyClaim    TaskType = "daily_claim"
	dailyClaimTaskID           = "daily-benefit-claim" // Keep existing UI switch IDs compatible.
	epWelfareDelivery          = "/v1/ops/delivery?channel=IDE"
	epWelfareClaim             = "/v1/ops/claim"
	epWelfareConfirm           = "/v1/ops/confirm"
)

var dailyClaimZone = time.FixedZone("Asia/Shanghai", 8*60*60)
var dailyClaimLocks sync.Map

func dailyClaimTask(enabled bool) ScheduleTask {
	return ScheduleTask{ID: dailyClaimTaskID, Type: TaskDailyClaim, Cron: "5-55/10 * * * *", Enabled: &enabled}
}

type welfareCampaign struct {
	ID            json.RawMessage `json:"campaignId"`
	Type          string          `json:"type"`
	Claimable     bool            `json:"claimable"`
	Status        string          `json:"status"`
	BenefitAmount float64         `json:"benefitAmount"`
	BenefitUnit   string          `json:"benefitUnit"`
}

func campaignKey(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

type welfareProgress struct {
	IdempotentKey string `json:"idempotent_key"`
	Claimed       bool   `json:"claimed"`
	Confirmed     bool   `json:"confirmed"`
}
type dailyClaimState struct {
	Version     int                        `json:"version"`
	Day         string                     `json:"day"`
	Attempts    int                        `json:"attempts"`
	LastAttempt time.Time                  `json:"last_attempt"`
	Accepted    bool                       `json:"confirmed"`
	Campaigns   map[string]welfareProgress `json:"campaigns"`
}

func dailyClaimPath(cfg *Config, file pluginapi.HostAuthFileEntry, cred *credential) (string, error) {
	if cfg.StateDir == "" && !filepath.IsAbs(file.Path) {
		return "", fmt.Errorf("账号缺少可持久化的认证文件路径")
	}
	identity := firstNonEmptyString(cred.DomainID+"/"+cred.UserID, file.Name, file.AuthIndex)
	if identity == "/" {
		identity = firstNonEmptyString(file.Name, file.AuthIndex)
	}
	if identity == "" {
		return "", fmt.Errorf("账号缺少稳定标识")
	}
	hash := sha256.Sum256([]byte(cfg.BaseURL + "\n" + identity))
	// New namespace: old benefit gateway "accepted" records are NOT evidence
	// of an ops welfare claim. Preserve old files but never use them to dedup.
	return cfg.statePath(filepath.Dir(file.Path), fmt.Sprintf("daily-welfare-%x.state", hash))
}

func welfareRequest(cfg *Config, cred *credential, method, path string, body any) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + path
	headers := map[string]string{"Content-Type": "application/json", "Agent-Type": "PromptCenter", "X-Language": cfg.Language}
	signed, err := signRequest(method, endpoint, headers, payload, cred, cfg.SignHost)
	if err != nil {
		return nil, fmt.Errorf("活动请求签名失败")
	}
	resp, err := hostHTTPDo(method, endpoint, signed, payload)
	if err != nil {
		return nil, fmt.Errorf("活动请求失败，请检查网络或登录状态")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("活动接口返回 HTTP %d", resp.StatusCode)
	}
	var result struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(resp.Body, &result) != nil || result.Code == nil || *result.Code != 0 {
		return nil, fmt.Errorf("活动接口未确认成功（需要 code=0），未记录为已领取")
	}
	return result.Data, nil
}

func fetchWelfareCampaigns(cfg *Config, cred *credential) ([]welfareCampaign, error) {
	b, err := welfareRequest(cfg, cred, http.MethodGet, epWelfareDelivery, nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Items *[]welfareCampaign `json:"items"`
	}
	if json.Unmarshal(b, &data) != nil || data.Items == nil {
		return nil, fmt.Errorf("活动列表响应缺少 items")
	}
	return *data.Items, nil
}

func welfareConfirmed(item welfareCampaign) bool {
	return !item.Claimable && (item.Status == "CONFIRMED" || item.Status == "CONSUMED")
}

func claimAccount(cfg *Config, file pluginapi.HostAuthFileEntry, cred *credential, now time.Time, manual bool) (string, error) {
	if cfg.APIMode == "native" {
		return "", fmt.Errorf("每日活动领取仅支持 agent 模式")
	}
	local := now.In(dailyClaimZone)
	if !manual && local.Hour() == 0 && local.Minute() < 5 {
		return "skipped", nil
	}
	path, err := dailyClaimPath(cfg, file, cred)
	if err != nil {
		return "", err
	}
	lock, _ := dailyClaimLocks.LoadOrStore(path, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	var state dailyClaimState
	err = readPluginState(path, &state)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		_, dateErr := time.Parse("2006-01-02", state.Day)
		if state.Version != 2 || dateErr != nil || state.Attempts < 0 || state.Attempts > 6 {
			return "", fmt.Errorf("每日活动记录无效，未发送领取请求")
		}
	}
	day := local.Format("2006-01-02")
	if state.Day > day {
		return "", fmt.Errorf("系统日期早于领取记录")
	}
	if state.Day != day {
		state = dailyClaimState{Version: 2, Day: day, Campaigns: map[string]welfareProgress{}}
	}
	if state.Campaigns == nil {
		state.Campaigns = map[string]welfareProgress{}
	}
	cred, err = prepareCredentialForUse(file.AuthIndex, cred)
	if err != nil {
		return "", fmt.Errorf("活动领取前刷新凭证失败")
	}
	items, err := fetchWelfareCampaigns(cfg, cred)
	if err != nil {
		return "", err
	}
	var daily []welfareCampaign
	for _, item := range items {
		// Do not claim registration/student/invitation rewards automatically.
		if item.Type == "USER_LOGIN" && item.BenefitUnit == "CREDIT" && campaignKey(item.ID) != "" {
			daily = append(daily, item)
		}
	}
	if len(daily) == 0 {
		return "not_eligible", nil
	}
	allConfirmed := true
	for _, item := range daily {
		if !welfareConfirmed(item) {
			allConfirmed = false
		}
	}
	if allConfirmed {
		// Delivery exposes no confirmation date. A previous login period may
		// still say CONFIRMED before today's event is delivered. Do not turn an
		// observed status into a local all-day skip; keep checking eligibility.
		return "already", nil
	}
	// Manual clicks always re-read upstream eligibility even after local success.
	// Rate limits protect only mutating attempts, not status/confirmation checks.
	if state.Attempts >= 6 || (!state.LastAttempt.IsZero() && now.Sub(state.LastAttempt) < 10*time.Minute) {
		return "skipped", nil
	}
	hasWork := false
	for _, item := range daily {
		if item.Claimable || item.Status == "CLAIMED" {
			hasWork = true
		}
	}
	if !hasWork {
		return "not_eligible", nil
	}
	state.Attempts++
	state.LastAttempt = now
	state.Accepted = false
	if err = savePluginState(path, state); err != nil {
		return "", err
	}
	for _, item := range daily {
		id := campaignKey(item.ID)
		if welfareConfirmed(item) {
			continue
		}
		progress := state.Campaigns[id]
		if item.Status == "CLAIMED" {
			progress.Claimed = true
		}
		if !progress.Claimed && item.Claimable {
			if progress.IdempotentKey == "" {
				progress.IdempotentKey = fmt.Sprintf("claim_%s_%d", id, now.UnixMilli())
			}
			state.Campaigns[id] = progress
			if err = savePluginState(path, state); err != nil {
				return "", err
			}
			b, errClaim := welfareRequest(cfg, cred, http.MethodPost, epWelfareClaim, map[string]any{"campaignId": item.ID, "idempotentKey": progress.IdempotentKey, "channel": "IDE"})
			if errClaim != nil {
				return "", errClaim
			}
			var claim struct {
				ID json.RawMessage `json:"campaignId"`
			}
			if json.Unmarshal(b, &claim) != nil || campaignKey(claim.ID) != id {
				return "", fmt.Errorf("领取响应活动 ID 不匹配，未进入确认阶段")
			}
			progress.Claimed = true
			state.Campaigns[id] = progress
			if err = savePluginState(path, state); err != nil {
				return "", err
			}
		}
		if progress.Claimed {
			if _, err = welfareRequest(cfg, cred, http.MethodPost, epWelfareConfirm, map[string]any{"campaignId": item.ID}); err != nil {
				return "", err
			}
		}
	}
	verified, err := fetchWelfareCampaigns(cfg, cred)
	if err != nil {
		return "", err
	}
	for _, item := range daily {
		found := false
		for _, v := range verified {
			if campaignKey(v.ID) == campaignKey(item.ID) && v.Type == "USER_LOGIN" && welfareConfirmed(v) {
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("请求已提交，但官方活动列表尚未确认到账；将重试确认，不显示签到成功")
		}
		p := state.Campaigns[campaignKey(item.ID)]
		p.Confirmed = true
		state.Campaigns[campaignKey(item.ID)] = p
	}
	state.Accepted = true
	if err = savePluginState(path, state); err != nil {
		return "", fmt.Errorf("官方已确认领取，但本地记录保存失败")
	}
	return "confirmed", nil
}

func runDailyClaimSweep(task ScheduleTask, now func() time.Time, manual bool) error {
	cfg := config()
	if !manual && (!cfg.DailyClaim.Enabled || !cfg.Schedule.Enabled || cfg.schedulePending || cfg.scheduleStateError != "") {
		return nil
	}
	files, err := hostAuthList()
	if err != nil {
		return fmt.Errorf("读取账号列表失败")
	}
	confirmed, already, skipped, unavailable, failed := 0, 0, 0, 0, 0
	seen := map[string]bool{}
	var lastErr error
	refreshFailed := false
	for _, file := range files {
		if file.Disabled || (normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID) {
			continue
		}
		storage, err := hostAuthGet(file.AuthIndex)
		if err != nil {
			failed++
			lastErr = fmt.Errorf("读取账号凭证失败")
			continue
		}
		cred, err := credentialFromStorage(storage)
		if err != nil || !cred.valid() {
			failed++
			lastErr = fmt.Errorf("账号凭证无效")
			continue
		}
		path, err := dailyClaimPath(cfg, file, cred)
		if err != nil {
			failed++
			lastErr = err
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		result, err := claimAccount(cfg, file, cred, now(), manual)
		if err != nil {
			failed++
			lastErr = err
			continue
		}
		switch result {
		case "confirmed":
			confirmed++
		case "already":
			already++
		case "not_eligible":
			unavailable++
		default:
			skipped++
		}
		if result == "confirmed" || (manual && result == "already") {
			if _, err := fetchQuotaSnapshot(file.AuthIndex, cred); err != nil {
				refreshFailed = true
			}
		}
	}
	if len(seen) == 0 && failed == 0 {
		return fmt.Errorf("没有可领取的 CodeArts 账号")
	}
	result := fmt.Sprintf("每日活动：官方确认 %d，已确认/已使用 %d，暂无可领取 %d，等待重试 %d，失败 %d。奖励计入套餐赠送积分，不增加福利模型 token 池。", confirmed, already, unavailable, skipped, failed)
	if refreshFailed {
		result += " 积分刷新失败，请点击刷新额度确认余额。"
	}
	if failed > 0 {
		return fmt.Errorf("%s %v", result, lastErr)
	}
	recordTaskResult(task.ID, result)
	return nil
}
