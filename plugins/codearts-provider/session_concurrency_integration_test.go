package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type integrationChatGate struct {
	entered chan struct{}
	release chan struct{}
}

// Exercise the real CPA ABI, including its credential cooldown handling, not
// just the plugin's executor. All credentials and upstream responses are fake.
func testCPAConcurrency(t *testing.T, base string, limit int, held *atomic.Pointer[integrationChatGate], chatCalls *atomic.Int32) {
	t.Helper()
	gate := &integrationChatGate{entered: make(chan struct{}, 64), release: make(chan struct{})}
	held.Store(gate)
	var once sync.Once
	release := func() { once.Do(func() { held.Store(nil); close(gate.release) }) }
	defer release()
	type result struct {
		status int
		body   []byte
		err    error
	}
	client := &http.Client{Timeout: 10 * time.Second}
	chat := func(stream bool) result {
		req, err := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"audit-model","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)))
		if err != nil {
			return result{err: err}
		}
		req.Header.Set("Authorization", "Bearer audit-client")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return result{err: err}
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return result{resp.StatusCode, body, err}
	}
	results := make(chan result, limit)
	for i := 0; i < limit; i++ {
		go func(stream bool) { results <- chat(stream) }(i%2 == 0)
	}
	for i := 0; i < limit; i++ {
		select {
		case <-gate.entered:
		case r := <-results:
			t.Fatalf("chat ended before %d requests entered: %d %s %v", limit, r.status, r.body, r.err)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d concurrent requests reached upstream", i, limit)
		}
	}
	before := chatCalls.Load()
	// Repeat while full: a mistaken cooldown would make the second response
	// auth_unavailable instead of the same request-scoped capacity conflict.
	for _, stream := range []bool{false, true} {
		r := chat(stream)
		// CPA forwards the plugin message/status but may omit its ABI error code.
		if r.err != nil || r.status != http.StatusConflict || !bytes.Contains(r.body, []byte("concurrent session limit")) {
			t.Fatalf("full account should return 409 without cooling: %d %s %v", r.status, r.body, r.err)
		}
	}
	if chatCalls.Load() != before {
		t.Fatal("over-cap chat reached upstream")
	}
	active := func() int {
		req, _ := http.NewRequest("GET", base+"/v0/management/codearts-provider/accounts", nil)
		req.Header.Set("Authorization", "Bearer audit-admin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var accounts struct {
			Accounts []struct {
				Concurrency sessionConcurrencyView `json:"concurrency"`
			} `json:"accounts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil || len(accounts.Accounts) != 1 {
			t.Fatalf("concurrency account view: %v", err)
		}
		view := accounts.Accounts[0].Concurrency
		if view.Limit != limit {
			t.Fatalf("effective cap = %d, want %d", view.Limit, limit)
		}
		return view.Active
	}
	if got := active(); got != limit {
		t.Fatalf("occupancy = %d, want %d", got, limit)
	}
	release()
	for i := 0; i < limit; i++ {
		select {
		case r := <-results:
			if r.err != nil || r.status != 200 || !bytes.Contains(r.body, []byte("hello from fixture")) {
				t.Fatalf("admitted request failed: %d %s %v", r.status, r.body, r.err)
			}
		case <-time.After(12 * time.Second):
			t.Fatal("admitted request did not finish")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for active() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("capacity leaked after completion")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := chat(false); r.err != nil || r.status != 200 {
		t.Fatalf("account remained unavailable after release: %d %s %v", r.status, r.body, r.err)
	}
}
