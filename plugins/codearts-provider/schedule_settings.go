package main

import (
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

var scheduleSettingsMu sync.Mutex
var scheduleRestoreStop chan struct{}

type scheduleSwitches struct {
	Version int             `json:"version"`
	Enabled *bool           `json:"enabled,omitempty"`
	Tasks   map[string]bool `json:"tasks,omitempty"`
}

type scheduleSwitchChange struct {
	Enabled *bool `json:"enabled"`
	Tasks   []struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	} `json:"tasks"`
}

func readScheduleSwitches(path string) (scheduleSwitches, error) {
	state := scheduleSwitches{}
	err := readPluginState(path, &state)
	if os.IsNotExist(err) {
		state.Version = 1
		err = nil
	}
	if err == nil && state.Version != 1 {
		err = fmt.Errorf("定时任务开关记录版本无效")
	}
	if state.Tasks == nil {
		state.Tasks = map[string]bool{}
	}
	return state, err
}

func applyScheduleSwitches(cfg *Config, state scheduleSwitches) *Config {
	updated := *cfg
	updated.Schedule.Tasks = append([]ScheduleTask(nil), cfg.Schedule.Tasks...)
	if len(updated.Schedule.Tasks) == 0 && !cfg.Schedule.DisableDefaults {
		updated.Schedule.Tasks = defaultScheduleTasks()
	}
	if state.Enabled != nil {
		updated.Schedule.Enabled = *state.Enabled
	}
	for i := range updated.Schedule.Tasks {
		if value, ok := state.Tasks[updated.Schedule.Tasks[i].ID]; ok {
			updated.Schedule.Tasks[i].Enabled = &value
		}
	}
	if value, ok := state.Tasks[dailyClaimTaskID]; ok {
		updated.DailyClaim.Enabled = value
	}
	return &updated
}

// Account loading can follow plugin.register. Defer cron until the auth path
// becomes discoverable, so startup never reactivates a task disabled in the UI.
func installScheduledConfig(cfg *Config) {
	scheduleSettingsMu.Lock()
	defer scheduleSettingsMu.Unlock()
	if scheduleRestoreStop != nil {
		close(scheduleRestoreStop)
		scheduleRestoreStop = nil
	}
	updated := *cfg
	updated.schedulePending, updated.scheduleStateError = true, ""
	currentConfig.Store(&updated)
	scheduler.stop()
	if restoreScheduledConfigLocked(&updated) {
		return
	}
	stop := make(chan struct{})
	scheduleRestoreStop = stop
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				scheduleSettingsMu.Lock()
				if scheduleRestoreStop != stop || config() != &updated {
					scheduleSettingsMu.Unlock()
					return
				}
				done := restoreScheduledConfigLocked(&updated)
				scheduleSettingsMu.Unlock()
				if done {
					return
				}
			}
		}
	}()
}

func restoreScheduledConfigLocked(cfg *Config) bool {
	directory := authDir()
	if cfg.StateDir == "" && !filepath.IsAbs(directory) {
		return false
	}
	path, err := cfg.statePath(directory, "schedule.state")
	if err != nil {
		updated := *cfg
		updated.schedulePending, updated.Schedule.Enabled, updated.scheduleStateError = false, false, err.Error()
		currentConfig.Store(&updated)
		return true
	}
	state, err := readScheduleSwitches(path)
	updated := applyScheduleSwitches(cfg, state)
	updated.scheduleStatePath, updated.schedulePending = path, false
	if err != nil {
		updated.scheduleStateError, updated.Schedule.Enabled = err.Error(), false
	}
	currentConfig.Store(updated)
	startScheduler(updated)
	return true
}

func stopScheduleSettings() {
	scheduleSettingsMu.Lock()
	defer scheduleSettingsMu.Unlock()
	if scheduleRestoreStop != nil {
		close(scheduleRestoreStop)
		scheduleRestoreStop = nil
	}
	scheduler.stop()
}

func scheduleView(cfg *Config) map[string]any {
	return map[string]any{
		"enabled":           cfg.Schedule.Enabled,
		"timezone":          firstNonEmptyString(cfg.Schedule.Timezone, time.Local.String()),
		"tasks":             describeTasks(cfg),
		"persistent":        cfg.scheduleStatePath != "" && cfg.scheduleStateError == "",
		"persistence_error": cfg.scheduleStateError,
		"pending":           cfg.schedulePending,
	}
}

func updateScheduleSwitches(change scheduleSwitchChange) pluginapi.ManagementResponse {
	scheduleSettingsMu.Lock()
	defer scheduleSettingsMu.Unlock()
	cfg := config()
	known := map[string]bool{}
	for _, task := range cfg.visibleScheduleTasks() {
		known[task.ID] = true
	}
	seen := map[string]bool{}
	for _, item := range change.Tasks {
		if !known[item.ID] || strings.TrimSpace(item.ID) == "" {
			return errorJSON(404, "unknown task "+item.ID)
		}
		if item.Enabled == nil || seen[item.ID] {
			return errorJSON(400, "每个任务必须指定 enabled，且 ID 不能重复")
		}
		if item.ID == dailyClaimTaskID && *item.Enabled && cfg.APIMode == "native" {
			return errorJSON(400, "每日领取仅支持 agent 模式")
		}
		seen[item.ID] = true
	}
	if change.Enabled == nil && len(change.Tasks) == 0 {
		return errorJSON(400, "请指定要修改的开关")
	}
	path, err := cfg.statePath(authDir(), "schedule.state")
	if err != nil {
		return errorJSON(409, err.Error())
	}
	state, err := readScheduleSwitches(path)
	if err != nil {
		return errorJSON(409, err.Error()+"；请修复或备份移走损坏记录后重试")
	}
	if change.Enabled != nil {
		state.Enabled = change.Enabled
	}
	for _, item := range change.Tasks {
		state.Tasks[item.ID] = *item.Enabled
	}
	updated := applyScheduleSwitches(cfg, state)
	updated.scheduleStatePath, updated.schedulePending, updated.scheduleStateError = path, false, ""
	// Disk first: a failed save must leave running flags intact.
	if err = savePluginState(path, state); err != nil {
		return errorJSON(500, err.Error())
	}
	currentConfig.Store(updated)
	startScheduler(updated)
	view := scheduleView(updated)
	view["success"] = true
	view["note"] = "开关已保存，重启后保留；已开始的任务会执行完毕。"
	b, _ := json.Marshal(view)
	return jsonResponse(http.StatusOK, b)
}
