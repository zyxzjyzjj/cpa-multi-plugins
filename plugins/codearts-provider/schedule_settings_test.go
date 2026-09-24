package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func switchRequest(body string) pluginapi.ManagementResponse {
	return handleScheduleConfig(pluginapi.ManagementRequest{Body: []byte(body)})
}

func TestScheduleSwitchesPersistAndRestore(t *testing.T) {
	_, file, _, _ := dailyClaimFixture(t)
	resp := switchRequest(`{"enabled":false,"tasks":[{"id":"token-renew","enabled":false},{"id":"daily-benefit-claim","enabled":true}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("save: %s", resp.Body)
	}
	if !config().DailyClaim.Enabled || config().Schedule.Enabled || config().scheduleTasks()[0].isEnabled() {
		t.Fatal("flags not applied")
	}
	path := filepath.Join(filepath.Dir(file.Path), pluginStateDir, "schedule.state")
	var state scheduleSwitches
	if err := readPluginState(path, &state); err != nil || state.Version != 1 {
		t.Fatalf("persistence: %v", err)
	}
	installScheduledConfig(defaultConfig())
	if config().Schedule.Enabled || config().scheduleTasks()[0].isEnabled() || !config().DailyClaim.Enabled || config().schedulePending {
		t.Fatal("restart/reconfigure lost switches")
	}
	if resp = switchRequest(`{"tasks":[{"id":"daily-benefit-claim","enabled":false}]}`); resp.StatusCode != 200 || config().DailyClaim.Enabled {
		t.Fatal("daily task cannot be disabled")
	}
}

func TestScheduleSwitchErrorsAreAtomic(t *testing.T) {
	cfg, file, _, _ := dailyClaimFixture(t)
	for _, body := range []string{`{"enabled":true,"tasks":[{"id":"unknown","enabled":true}]}`, `{"tasks":[{"id":"token-renew"}]}`, `{}`, `{"tasks":[{"id":"token-renew","enabled":true},{"id":"token-renew","enabled":false}]}`} {
		if resp := switchRequest(body); resp.StatusCode < 400 || config() != cfg {
			t.Fatalf("partial update: %s", resp.Body)
		}
	}
	dir := filepath.Join(filepath.Dir(file.Path), pluginStateDir)
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if resp := switchRequest(`{"enabled":true}`); resp.StatusCode < 400 || config() != cfg {
		t.Fatal("failed save changed runtime")
	}
}

func TestScheduleRestoreWaitsForAuthPathAndCorruptionPauses(t *testing.T) {
	_, file, _, _ := dailyClaimFixture(t)
	restore := setHostCall(func(string, any) (json.RawMessage, error) { return json.RawMessage(`{"files":[]}`), nil })
	installScheduledConfig(defaultConfig())
	if !config().schedulePending || len(scheduler.nextRuns()) != 0 {
		t.Fatal("cron started before restoring persistent flags")
	}
	restore()
	path, _ := pluginStatePath(filepath.Dir(file.Path), "schedule.state")
	if err := savePluginState(path, map[string]any{"version": 999}); err != nil {
		t.Fatal(err)
	}
	scheduleSettingsMu.Lock()
	done := restoreScheduledConfigLocked(config())
	scheduleSettingsMu.Unlock()
	if !done || config().schedulePending || config().Schedule.Enabled || config().scheduleStateError == "" {
		t.Fatal("corrupt switch state did not fail closed")
	}
}

func TestDailyTaskReservedIDRejected(t *testing.T) {
	request, _ := json.Marshal(map[string]any{"config_yaml": []byte("schedule:\n  tasks:\n    - id: daily-benefit-claim\n      type: http\n")})
	if _, err := parseConfig(request); err == nil {
		t.Fatal("built-in ID may redirect a claim to a custom task")
	}
}

func TestExplicitStateDirectorySurvivesAuthSpoolReset(t *testing.T) {
	cfg, file, cred, calls := dailyClaimFixture(t)
	cfg.StateDir = t.TempDir() // A separate volume, outside the recreated auth spool.
	if resp := switchRequest(`{"enabled":false,"tasks":[{"id":"token-renew","enabled":false}]}`); resp.StatusCode != 200 {
		t.Fatalf("save: %s", resp.Body)
	}
	now := time.Now()
	if _, err := claimAccount(cfg, file, cred, now, true); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(file.Path)); err != nil {
		t.Fatal(err)
	} // fixture-owned t.TempDir only
	restoreEmpty := setHostCall(func(string, any) (json.RawMessage, error) { return json.RawMessage(`{"files":[]}`), nil })
	defer restoreEmpty()
	fresh := defaultConfig()
	fresh.StateDir = cfg.StateDir
	installScheduledConfig(fresh)
	restoreEmpty()
	if config().schedulePending || config().Schedule.Enabled || config().scheduleTasks()[0].isEnabled() {
		t.Fatal("spool reset lost persistent switches")
	}
	if result, err := claimAccount(config(), file, cred, now.Add(time.Minute), true); err != nil || result != "already" || calls.Load() != 1 {
		t.Fatal("spool reset lost successful claim")
	}
}
