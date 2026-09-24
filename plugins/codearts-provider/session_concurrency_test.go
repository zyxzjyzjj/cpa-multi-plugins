package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func concurrencyCredential(user string) *credential {
	return &credential{AccessKeyID: "fixture-ak-" + user, SecretAccessKey: "fixture-sk", DomainID: "fixture-domain", UserID: user}
}

func holdPermits(t *testing.T, cfg *Config, cred *credential, n int) []*sessionPermit {
	t.Helper()
	out := make([]*sessionPermit, 0, n)
	for i := 0; i < n; i++ {
		p, err := acquireSessionPermit(cfg, executorRequest{}, cred)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
		t.Cleanup(p.release)
	}
	return out
}

func TestSessionConcurrencyDefaultThreeAndPerAccountFive(t *testing.T) {
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	free, paid := concurrencyCredential("free"), concurrencyCredential("paid")
	path, _ := sessionLimitPath(cfg, sessionCapacityKey(cfg, credentialRefreshKey(paid)), "")
	if err := savePluginState(path, sessionLimitState{Version: 1, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	freePermits := holdPermits(t, cfg, free, 3)
	holdPermits(t, cfg, paid, 5)
	for _, cred := range []*credential{free, paid} {
		if _, err := acquireSessionPermit(cfg, executorRequest{}, cred); err == nil {
			t.Fatal("over-limit request accepted")
		}
	}
	rotated := *free
	rotated.AccessKeyID = "rotated-ak"
	rotated.SecurityToken = "rotated-sts"
	if _, err := acquireSessionPermit(cfg, executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "duplicate-login"}}, &rotated); err == nil {
		t.Fatal("rotation/duplicate login bypassed cap")
	}
	freePermits[0].release()
	freePermits[0].release()
	p, err := acquireSessionPermit(cfg, executorRequest{}, free)
	if err != nil {
		t.Fatal(err)
	}
	p.release()
	if v := accountSessionConcurrency(cfg, paid, ""); v.Active != 5 || v.Limit != 5 {
		t.Fatalf("bad account view: %+v", v)
	}
}

func TestSessionConcurrencyAtomicUnderContention(t *testing.T) {
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cred := concurrencyCredential("race")
	results := make(chan *sessionPermit, 40)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			p, _ := acquireSessionPermit(cfg, executorRequest{}, cred)
			if p != nil {
				results <- p
			}
		})
	}
	wg.Wait()
	close(results)
	count := 0
	for p := range results {
		count++
		p.release()
	}
	if count != 3 {
		t.Fatalf("admitted %d concurrent requests, want 3", count)
	}
	if v := accountSessionConcurrency(cfg, cred, ""); v.Active != 0 {
		t.Fatal("permits leaked")
	}
}

func concurrencySettingsFixture(t *testing.T) (*Config, *credential, func(string) pluginapi.ManagementResponse) {
	t.Helper()
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cred := concurrencyCredential("settings")
	useModelTestConfig(t, cfg)
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{{AuthIndex: "fixture", ID: "fixture.json", Name: "fixture.json", Provider: providerID}}})
		case "host.auth.get":
			doc, _ := buildAuthFileDocument(*cred)
			return json.Marshal(map[string]any{"json": json.RawMessage(doc)})
		default:
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
	})
	return cfg, cred, func(body string) pluginapi.ManagementResponse {
		return handleSessionConcurrency(pluginapi.ManagementRequest{Body: []byte(body)})
	}
}

func TestSessionConcurrencyPersistsLowersWithoutCancellingAndRestoresDefault(t *testing.T) {
	cfg, cred, save := concurrencySettingsFixture(t)
	if r := save(`{"auth_index":"fixture","limit":5}`); r.StatusCode != 200 {
		t.Fatalf("save: %s", r.Body)
	}
	permits := holdPermits(t, cfg, cred, 5)
	if r := save(`{"auth_index":"fixture","limit":3}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if v := accountSessionConcurrency(cfg, cred, ""); v.Active != 5 || v.Limit != 3 {
		t.Fatal("lowering killed active sessions")
	}
	for i := 0; i < 2; i++ {
		permits[i].release()
		if _, err := acquireSessionPermit(cfg, executorRequest{}, cred); err == nil {
			t.Fatal("accepted while at or above lowered cap")
		}
	}
	permits[2].release()
	p, err := acquireSessionPermit(cfg, executorRequest{}, cred)
	if err != nil {
		t.Fatal(err)
	}
	p.release()
	restarted := defaultConfig()
	restarted.StateDir = cfg.StateDir
	if v := accountSessionConcurrency(restarted, cred, ""); v.Override != 3 {
		t.Fatal("restart lost override")
	}
	if r := save(`{"auth_index":"fixture","limit":0}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	restarted.ChatSessionConcurrency = 5
	if v := accountSessionConcurrency(restarted, cred, ""); v.Limit != 5 || v.Override != 0 {
		t.Fatal("inherit default did not work")
	}
}

func TestSessionConcurrencyInvalidSettingsAndStorageFailClosed(t *testing.T) {
	cfg, cred, save := concurrencySettingsFixture(t)
	for _, body := range []string{`{}`, `{"auth_index":"fixture","limit":-1}`, `{"auth_index":"fixture","limit":65}`, `{"auth_index":"fixture","limit":3.5}`, `{"auth_index":"missing","limit":5}`} {
		if r := save(body); r.StatusCode < 400 {
			t.Fatalf("invalid settings accepted: %s", body)
		}
	}
	path, _ := sessionLimitPath(cfg, sessionCapacityKey(cfg, credentialRefreshKey(cred)), "")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireSessionPermit(cfg, executorRequest{}, cred); err == nil {
		t.Fatal("corrupt override bypassed cap")
	}
	if r := save(`{"auth_index":"fixture","limit":5}`); r.StatusCode != 200 {
		t.Fatal("explicit save did not repair invalid state")
	}
	// A blocked destination must not produce a false successful save.
	other := *cfg
	other.StateDir = filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(other.StateDir, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	currentConfig.Store(&other)
	if r := save(`{"auth_index":"fixture","limit":3}`); r.StatusCode < 400 {
		t.Fatal("unwritable directory reported saved")
	}
}

func TestSessionConcurrencyConfigValidation(t *testing.T) {
	for _, n := range []int{0, -1, 65} {
		req, _ := json.Marshal(map[string]any{"config_yaml": []byte(fmt.Sprintf("chat_session_concurrency: %d", n))})
		if _, err := parseConfig(req); err == nil {
			t.Fatalf("accepted invalid default %d", n)
		}
	}
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("chat_session_concurrency: 5")})
	cfg, err := parseConfig(req)
	if err != nil || defaultSessionLimit(cfg) != 5 {
		t.Fatalf("valid global limit failed: %v", err)
	}
}

func TestSessionConcurrencyAuthDirectoryFallback(t *testing.T) {
	cfg := defaultConfig()
	authPath := filepath.Join(t.TempDir(), "account.json")
	cred := concurrencyCredential("auth-path")
	key := sessionCapacityKey(cfg, credentialRefreshKey(cred))
	path, err := sessionLimitPath(cfg, key, authPath)
	if err != nil || filepath.Dir(path) != filepath.Join(filepath.Dir(authPath), pluginStateDir) {
		t.Fatalf("incorrect auth directory fallback: %s %v", path, err)
	}
	if err := savePluginState(path, sessionLimitState{Version: 1, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthAttributes: map[string]string{"path": authPath}}}
	p, err := acquireSessionPermit(cfg, req, cred)
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	if _, err := acquireSessionPermit(cfg, req, cred); err == nil {
		t.Fatal("executor ignored override stored beside auth files")
	}
	cfg.scheduleStatePath = filepath.Join(filepath.Dir(path), "schedule.state")
	if view, err := readSessionLimit(cfg, key, ""); err != nil || view.Limit != 1 {
		t.Fatalf("resolved state directory lost the override: %+v %v", view, err)
	}
}

func TestSessionSchedulerSkipsFullPreferredAccount(t *testing.T) {
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.Scheduler = SchedulerConfig{Strategy: "preferred", PreferredAuthID: "free"}
	useModelTestConfig(t, cfg)
	free, paid := concurrencyCredential("picker-free"), concurrencyCredential("picker-paid")
	holdPermits(t, cfg, free, 3)
	req := pluginapi.SchedulerPickRequest{Provider: providerID, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "free", Metadata: credentialAuthMetadata(free)}, {ID: "paid", Metadata: credentialAuthMetadata(paid)},
	}}
	raw, err := schedulerPick(mustMarshal(t, req))
	resp := testEnvelopeResult[pluginapi.SchedulerPickResponse](t, raw, err)
	if resp.AuthID != "paid" {
		t.Fatalf("picked full account: %+v", resp)
	}
}

func TestExecutorRejectsOverCapWithoutContactingUpstream(t *testing.T) {
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.ChatSessionHeartbeat = false
	useModelTestConfig(t, cfg)
	cred := concurrencyCredential("executor-full")
	holdPermits(t, cfg, cred, 3)
	testHost(t, func(string, any) (json.RawMessage, error) {
		t.Error("over-cap request contacted host/upstream")
		return nil, errors.New("unexpected")
	})
	for _, mode := range []string{"completion", "inline-stream", "async-stream"} {
		env := executeErrorFixture(t, mode, *cred)
		if env.Error == nil || env.Error.HTTPStatus != 409 || env.Error.Code != "session_concurrency_limit" {
			t.Fatalf("bad admission error: %+v", env)
		}
	}
}

func TestExecutorAsyncPermitHeldUntilStreamCloses(t *testing.T) {
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.DiscoverModels = false
	cfg.ChatSessionHeartbeat = false
	cfg.ChatSessionConcurrency = 1
	useModelTestConfig(t, cfg)
	cred := concurrencyCredential("stream-lifetime")
	entered := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	answering := true
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			return json.Marshal(hostHTTPStreamOpen{StatusCode: 200, StreamID: "limit-upstream"})
		case "host.http.stream_read":
			// The executor only returns once the stream has proved it is answering,
			// so the first read has to deliver content; the pump then parks on the
			// second read with the permit still held.
			if answering {
				answering = false
				close(entered)
				return json.Marshal(hostHTTPStreamChunk{Payload: []byte(answerFrame)})
			}
			<-unblock
			return json.Marshal(hostHTTPStreamChunk{Done: true})
		case "host.http.stream_close":
			once.Do(func() { close(unblock) })
			return json.RawMessage(`{}`), nil
		case "host.stream.emit":
			return json.RawMessage(`{}`), nil
		case "host.stream.close":
			close(done)
			return json.RawMessage(`{}`), nil
		}
		return nil, fmt.Errorf("unexpected callback %s", method)
	})
	req := executorRequest{StreamID: "limit-client"}
	req.Model = "fixture-model"
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	req.StorageJSON = mustMarshal(t, map[string]any{storageKey: cred})
	raw, err := executorExecuteStream(mustMarshal(t, req))
	_ = testEnvelopeResult[map[string]any](t, raw, err)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	if v := accountSessionConcurrency(cfg, cred, ""); v.Active != 1 {
		t.Fatal("stream released permit on initial ABI return")
	}
	stop, ok := activeStreams.Load(req.StreamID)
	if !ok {
		t.Fatal("active stream was not registered")
	}
	stop.(func())()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not close")
	}
	if v := accountSessionConcurrency(cfg, cred, ""); v.Active != 0 {
		t.Fatal("stream cleanup leaked permit")
	}
}
