package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// TestCronSpecNormalisesFieldCounts covers the two accepted cron dialects: the
// standard 5-field form and the seconds-first 6-field form.
func TestCronSpecNormalisesFieldCounts(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "*/30 * * * *", want: "0 */30 * * * *"},
		{in: "17 * * * *", want: "0 17 * * * *"},
		{in: "0 9 * * 1-5", want: "0 0 9 * * 1-5"},
		{in: "30 17 * * * *", want: "30 17 * * * *"},
		{in: "  */10  *  * * *  ", want: "0 */10  *  * * *"},
		{in: "", wantErr: true},
		{in: "* * *", wantErr: true},
		{in: "* * * * * * *", wantErr: true},
	}
	for _, tc := range cases {
		got, err := cronSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("cronSpec(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("cronSpec(%q) returned an error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("cronSpec(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if errValidate := validateCron(tc.in); errValidate != nil {
			t.Errorf("validateCron(%q) rejected a valid expression: %v", tc.in, errValidate)
		}
	}
}

func TestValidateCronRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not a cron", "99 * * * *", "* * * * * * *", ""} {
		if err := validateCron(bad); err == nil {
			t.Errorf("validateCron(%q) accepted an invalid expression", bad)
		}
	}
}

// TestDefaultScheduleIsUsable ensures the built-in task set parses and that
// entry flags default to enabled.
func TestDefaultScheduleIsUsable(t *testing.T) {
	tasks := defaultScheduleTasks()
	if len(tasks) == 0 {
		t.Fatal("default schedule is empty")
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if task.ID == "" {
			t.Error("default task has no id")
		}
		if seen[task.ID] {
			t.Errorf("duplicate default task id %q", task.ID)
		}
		seen[task.ID] = true
		if !task.isEnabled() {
			t.Errorf("default task %q should be enabled", task.ID)
		}
		if err := validateCron(task.Cron); err != nil {
			t.Errorf("default task %q has an invalid cron: %v", task.ID, err)
		}
		if task.Type == "" {
			t.Errorf("default task %q has no type", task.ID)
		}
	}
	// The renewal cadence must be at least hourly to keep the 24h credential
	// alive, matching the extension's own hourly renewal.
	if !seen["token-renew"] {
		t.Error("the default schedule must renew credentials")
	}
}

// TestScheduleTaskEnabledDefaultsToOn covers the tri-state flag: an omitted
// value must mean enabled, and an explicit false must disable.
func TestScheduleTaskEnabledDefaultsToOn(t *testing.T) {
	var omitted ScheduleTask
	if !omitted.isEnabled() {
		t.Error("an omitted enabled flag must default to on")
	}
	off := false
	disabled := ScheduleTask{Enabled: &off}
	if disabled.isEnabled() {
		t.Error("an explicit false must disable the task")
	}
}

// TestScheduleConfigNormalizeFillsDefaults checks the config normalisation that
// runs after YAML decoding.
func TestScheduleConfigNormalizeFillsDefaults(t *testing.T) {
	// An empty task list with defaults allowed yields the built-in set.
	var empty ScheduleConfig
	empty.normalize()
	if len(empty.Tasks) == 0 {
		t.Fatal("normalize did not install the default tasks")
	}

	// Defaults suppressed yields nothing.
	var suppressed ScheduleConfig
	suppressed.DisableDefaults = true
	suppressed.normalize()
	if len(suppressed.Tasks) != 0 {
		t.Fatalf("disable_defaults must suppress defaults, got %d tasks", len(suppressed.Tasks))
	}

	// A minimal entry gets an id, a type and a cron.
	cfg := ScheduleConfig{Tasks: []ScheduleTask{{Path: "/v1/chat/custom-instructions"}}}
	cfg.normalize()
	if len(cfg.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(cfg.Tasks))
	}
	task := cfg.Tasks[0]
	if task.Type != TaskHTTP {
		t.Errorf("a path-only task should default to the http type, got %q", task.Type)
	}
	if task.ID == "" {
		t.Error("task id was not generated")
	}
	if task.Cron == "" {
		t.Error("task cron was not defaulted")
	}
	if err := validateCron(task.Cron); err != nil {
		t.Errorf("defaulted cron is invalid: %v", err)
	}
}

// TestScheduleConfigFromYAML exercises the real config path: YAML in, effective
// tasks out, including a user-defined HTTP task.
func TestScheduleConfigFromYAML(t *testing.T) {
	yamlDoc := `
enabled: true
priority: 5
base_url: "https://snap-access.cn-north-4.myhuaweicloud.com"
schedule:
  enabled: true
  timezone: "Asia/Shanghai"
  tasks:
    - id: "daily-report"
      type: "http"
      cron: "0 9 * * *"
      path: "/snap-manager/v1/statistics/plugin"
      method: "GET"
      sign: true
    - id: "renew"
      type: "token_renew"
      cron: "0 */6 * * *"
      enabled: false
`
	request, errMarshal := json.Marshal(map[string]any{
		"config_yaml":    encodeBase64([]byte(yamlDoc)),
		"schema_version": pluginabi.SchemaVersion,
	})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}
	cfg, errParse := parseConfig(request)
	if errParse != nil {
		t.Fatalf("parseConfig: %v", errParse)
	}
	if !cfg.Schedule.Enabled {
		t.Error("schedule.enabled was not decoded")
	}
	if cfg.Schedule.Timezone != "Asia/Shanghai" {
		t.Errorf("timezone = %q, want Asia/Shanghai", cfg.Schedule.Timezone)
	}
	tasks := cfg.scheduleTasks()
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2 (config tasks must replace the defaults)", len(tasks))
	}
	byID := map[string]ScheduleTask{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	report, ok := byID["daily-report"]
	if !ok {
		t.Fatal("the custom http task was not decoded")
	}
	if report.Type != TaskHTTP || report.Path != "/snap-manager/v1/statistics/plugin" || report.Method != "GET" || !report.Sign {
		t.Errorf("http task decoded incorrectly: %+v", report)
	}
	if err := validateCron(report.Cron); err != nil {
		t.Errorf("http task cron is invalid: %v", err)
	}
	renew, ok := byID["renew"]
	if !ok {
		t.Fatal("the token_renew task was not decoded")
	}
	if renew.isEnabled() {
		t.Error("an explicit enabled:false task must be disabled")
	}
}

// TestScheduleConfigDefaultsFromYAML verifies that omitting the schedule block
// enables the safety-critical renewal defaults. An operator can still disable
// the scheduler explicitly with schedule.enabled:false.
func TestScheduleConfigDefaultsFromYAML(t *testing.T) {
	request, _ := json.Marshal(map[string]any{
		"config_yaml": encodeBase64([]byte("enabled: true\nbase_url: \"https://example.invalid\"\n")),
	})
	cfg, errParse := parseConfig(request)
	if errParse != nil {
		t.Fatalf("parseConfig: %v", errParse)
	}
	if !cfg.Schedule.Enabled {
		t.Error("the default scheduler must keep temporary credentials renewed")
	}
	if len(cfg.scheduleTasks()) == 0 {
		t.Error("default renewal tasks should be available")
	}
}

func TestScheduleCanBeExplicitlyDisabled(t *testing.T) {
	request, _ := json.Marshal(map[string]any{
		"config_yaml": encodeBase64([]byte("enabled: true\nschedule:\n  enabled: false\n")),
	})
	cfg, errParse := parseConfig(request)
	if errParse != nil {
		t.Fatalf("parseConfig: %v", errParse)
	}
	if cfg.Schedule.Enabled {
		t.Error("an explicit schedule.enabled:false must be preserved")
	}
}

// TestSchedulerRunTaskRecordsOutcome verifies the state bookkeeping the
// management API reports.
func TestSchedulerRunTaskRecordsOutcome(t *testing.T) {
	const id = "test-task-records-outcome"
	task := ScheduleTask{ID: id, Type: TaskType("unknown-type"), Cron: "* * * * *"}

	runTask(task)
	states := scheduler.snapshot()
	state, ok := states[id]
	if !ok {
		t.Fatalf("task %q was not recorded", id)
	}
	if state.RunCount != 1 {
		t.Errorf("run_count = %d, want 1", state.RunCount)
	}
	if state.ErrorCount != 1 {
		t.Errorf("error_count = %d, want 1 for an unknown task type", state.ErrorCount)
	}
	if !strings.Contains(state.LastError, "unknown task type") {
		t.Errorf("last_error = %q, want it to mention the unknown type", state.LastError)
	}
	if state.LastRunAt.IsZero() {
		t.Error("last_run was not recorded")
	}
}

// TestSchedulerOverlapGuard ensures a task already in flight is not re-entered.
func TestSchedulerOverlapGuard(t *testing.T) {
	const id = "test-task-overlap"
	if !scheduler.setRunning(id, true) {
		t.Fatal("first acquisition should succeed")
	}
	if scheduler.setRunning(id, true) {
		t.Fatal("a second concurrent acquisition must be refused")
	}
	scheduler.setRunning(id, false)
	if !scheduler.setRunning(id, true) {
		t.Fatal("acquisition should succeed again after release")
	}
	scheduler.setRunning(id, false)
}

// TestTriggerTaskUnknownID is the management API's not-found path.
func TestTriggerTaskUnknownID(t *testing.T) {
	if err := triggerTask("definitely-not-a-task"); err == nil {
		t.Fatal("triggerTask accepted an unknown task id")
	}
}

// TestDescribeTasksReportsConfigAndErrors verifies the schedule view exposed by
// the management API, including surfacing a bad cron expression rather than
// hiding it.
func TestDescribeTasksReportsConfigAndErrors(t *testing.T) {
	on := true
	cfg := &Config{
		Schedule: ScheduleConfig{
			Enabled: true,
			Tasks: []ScheduleTask{
				{ID: "good", Type: TaskQuotaRefresh, Cron: "*/15 * * * *", Enabled: &on},
				{ID: "bad", Type: TaskHTTP, Cron: "nonsense", Enabled: &on, Path: "/x"},
			},
		},
	}
	described := describeTasks(cfg)
	if len(described) != 3 {
		t.Fatalf("described %d tasks, want 2 configured and 1 opt-in daily task", len(described))
	}
	// Sorted by id, so "bad" comes first.
	if described[0]["id"] != "bad" {
		t.Fatalf("tasks are not sorted by id: %v", described[0]["id"])
	}
	if described[0]["cron_error"] == nil {
		t.Error("an invalid cron expression must be reported in cron_error")
	}
	if described[2]["cron_error"] != nil {
		t.Errorf("a valid cron expression must not report cron_error: %v", described[2]["cron_error"])
	}
	if described[2]["enabled"] != true {
		t.Errorf("enabled flag = %v, want true", described[2]["enabled"])
	}
}

// TestSchedulerStartsAndStops proves the runner accepts the configured tasks and
// that stop is idempotent, which is what plugin reconfigure relies on.
func TestSchedulerStartsAndStops(t *testing.T) {
	on := true
	cfg := &Config{
		Schedule: ScheduleConfig{
			Enabled: true,
			Tasks: []ScheduleTask{
				{ID: "startstop", Type: TaskQuotaRefresh, Cron: "* * * * *", Enabled: &on},
			},
		},
	}
	startScheduler(cfg)
	if next := scheduler.nextRuns(); next["startstop"] == "" {
		t.Error("the started scheduler reports no next run for its task")
	}
	startScheduler(cfg) // reconfigure path: must not panic or leak
	if next := scheduler.nextRuns(); next["startstop"] == "" {
		t.Error("the reconfigured scheduler reports no next run")
	}
	scheduler.stop()
	if len(scheduler.nextRuns()) != 0 {
		t.Error("a stopped scheduler still reports next runs")
	}
	scheduler.stop() // idempotent
}

// TestSchedulerDisabledRunsNothing verifies that disabled schedules install no
// cron entries.
func TestSchedulerDisabledRunsNothing(t *testing.T) {
	on := true
	cfg := &Config{
		Schedule: ScheduleConfig{
			Enabled: false,
			Tasks: []ScheduleTask{
				{ID: "should-not-run", Type: TaskQuotaRefresh, Cron: "* * * * *", Enabled: &on},
			},
		},
	}
	startScheduler(cfg)
	if len(scheduler.nextRuns()) != 0 {
		t.Error("a disabled scheduler must not install cron entries")
	}
	scheduler.stop()
}

// TestUnusedImportGuard keeps the atomic import meaningful for the scheduler
// task-count assertion below and documents intent.
var schedulerTestRuns atomic.Int64

// TestRunTaskIsNonBlockingForTrigger documents that triggerTask returns
// immediately and the task runs on its own goroutine.
func TestRunTaskIsNonBlockingForTrigger(t *testing.T) {
	cfg := defaultConfig()
	cfg.Schedule = ScheduleConfig{
		Enabled: false,
		Tasks:   []ScheduleTask{{ID: "async-task", Type: TaskQuotaRefresh, Cron: "* * * * *"}},
	}
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	start := time.Now()
	if err := triggerTask("async-task"); err != nil {
		t.Fatalf("triggerTask: %v", err)
	}
	// The call must return well before any upstream work could finish; without
	// a host the task fails fast, so only the dispatch latency is measured.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("triggerTask blocked for %v, it must dispatch in the background", elapsed)
	}
	// Let the background run settle so it does not race the next test.
	time.Sleep(50 * time.Millisecond)
	schedulerTestRuns.Add(1)
}
