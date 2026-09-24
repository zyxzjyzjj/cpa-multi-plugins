package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	chatSessionHeartbeatPath     = "/snap-manager/v1/chat-session/heartbeat"
	chatSessionHeartbeatInterval = 30 * time.Second
)

// Only sessions created by this plugin are kept here. A caller-supplied session
// ID must never be used: sending idle for it could release another client's chat.
var activeChatSessions sync.Map

type chatSession struct {
	id         string
	baseURL    string
	language   string
	credential credential
	callbackID string
	stop       chan struct{}
	done       chan struct{}
	stopOnce   sync.Once
}

// beginChatSession reserves one request-owned upstream session before chat
// starts. A non-200 busy response is returned intact for the executor's normal
// upstream error handling. No renewal goroutine exists when acquisition fails.
func beginChatSession(cfg *Config, req executorRequest, cred *credential) (*chatSession, *hostHTTPResponse, error) {
	return beginChatSessionWithInterval(cfg, req, cred, chatSessionHeartbeatInterval)
}

// The interval argument allows lifecycle tests to exercise renewal without
// mutating a global timer shared by concurrent requests.
func beginChatSessionWithInterval(cfg *Config, req executorRequest, cred *credential, interval time.Duration) (*chatSession, *hostHTTPResponse, error) {
	if cfg == nil || !cfg.ChatSessionHeartbeat || cfg.APIMode == "native" || !cred.valid() {
		return nil, nil, nil
	}
	if interval <= 0 {
		interval = chatSessionHeartbeatInterval
	}
	session := &chatSession{
		id:         strings.ReplaceAll(randomUUIDv4(), "-", ""),
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		language:   cfg.Language,
		callbackID: req.HostCallbackID,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		// The heartbeat needs only the temporary signing credentials. Do not
		// retain OAuth refresh tokens, proof keys or unnecessary identity data.
		credential: credential{
			AccessKeyID:     cred.AccessKeyID,
			SecretAccessKey: cred.SecretAccessKey,
			SecurityToken:   cred.SecurityToken,
		},
	}
	response, attempted, err := session.heartbeat("busy", session.callbackID)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || !chatSessionHeartbeatAccepted(response) {
		// A transport failure can occur after the upstream acquired the slot.
		// Release only our freshly generated ID, even when acceptance is unknown.
		if attempted {
			session.release()
		}
		if err != nil {
			return nil, nil, err
		}
		if response != nil && response.StatusCode != http.StatusOK {
			return nil, response, nil
		}
		return nil, nil, fmt.Errorf("CodeArts chat-session heartbeat did not acknowledge busy status")
	}
	activeChatSessions.Store(session.id, session)
	go session.run(interval)
	return session, nil, nil
}

func (s *chatSession) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Stop waits until any in-flight busy heartbeat has completed and then sends
// idle exactly once. This ordering prevents a delayed busy response from
// reactivating the session after cleanup. Repeated/concurrent calls are safe.
// The host C ABI provides no per-callback HTTP deadline: cleanup waits for the
// host transport rather than abandoning a callback during plugin quiescence.
func (s *chatSession) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

func (s *chatSession) run(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		s.stopOnce.Do(func() { close(s.stop) })
		s.release()
		activeChatSessions.Delete(s.id)
		close(s.done)
	}()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			// Prefer shutdown if it raced with a ready ticker.
			select {
			case <-s.stop:
				return
			default:
			}
			response, _, err := s.heartbeat("busy", s.callbackID)
			if err != nil || !chatSessionHeartbeatAccepted(response) {
				// A failed renewal does not mean the chat has ended. Keep the
				// slot busy until executor completion/cancellation calls Stop;
				// otherwise a transient heartbeat error could release a live chat.
				logWarn("CodeArts chat-session heartbeat was not acknowledged", nil)
			}
		}
	}
}

func (s *chatSession) heartbeat(status, callbackID string) (*hostHTTPResponse, bool, error) {
	endpoint := s.baseURL + chatSessionHeartbeatPath + "?status=" + url.QueryEscape(status)
	body := []byte(`{}`)
	headers, err := signRequest(http.MethodPut, endpoint, map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "application/json",
		"X-Language":      s.language,
		"x-client-type":   "kernel",
		"user-session-id": s.id,
		"x-snap-traceid":  strings.ReplaceAll(randomUUIDv4(), "-", ""),
	}, body, &s.credential, true)
	if err != nil {
		return nil, false, fmt.Errorf("could not sign CodeArts chat-session heartbeat")
	}
	response, err := hostHTTPDoContext(callbackID, http.MethodPut, endpoint, headers, body)
	if err != nil {
		// Host transport errors can contain credential-bearing URLs or proxy
		// diagnostics. Neither headers nor raw transport errors belong in logs.
		return nil, true, fmt.Errorf("CodeArts chat-session heartbeat transport failed")
	}
	return response, true, nil
}

func chatSessionHeartbeatAccepted(response *hostHTTPResponse) bool {
	if response == nil || response.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(response.Body, &payload) == nil && payload.Status == "ok"
}

func (s *chatSession) release() {
	// The client may have disconnected already. A detached callback is needed
	// for cleanup, otherwise the host cancels idle along with the chat request.
	response, _, err := s.heartbeat("idle", "")
	if err != nil || !chatSessionHeartbeatAccepted(response) {
		logWarn("CodeArts chat-session idle notification was not acknowledged", nil)
	}
}

func stopAllChatSessions() {
	activeChatSessions.Range(func(_, value any) bool {
		if session, ok := value.(*chatSession); ok {
			session.Stop()
		}
		return true
	})
}
