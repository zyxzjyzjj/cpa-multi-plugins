package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// agentModePath is the OpenAI-compatible endpoint the official extension uses in
// agent mode.
const agentModePath = "/api/v2/chat/completions"

// nativeChatPath is the proprietary CodeArts chat endpoint.
const nativeChatPath = "/v1/chat/chat"

// activeStreams tracks in-flight executor streams so shutdown can stop them.
var activeStreams sync.Map

// executorRequest mirrors the ExecutorRequest JSON the host sends, including the
// plugin-only stream identifier the host adds for streaming calls.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
	ChatSessionID  string `json:"-"`
}

// executorExecute handles the non-streaming execution path. It always streams
// upstream and aggregates, because the CodeArts native protocol only offers a
// streaming variant.
func executorExecute(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := config()
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return failEnvelope("invalid_credential", "stored credential could not be decoded: "+errCred.Error(), http.StatusUnauthorized)
	}
	if !cred.valid() && !cfg.InsistMissingCredentials {
		return failEnvelope(
			"missing_credential",
			"no CodeArts Doer credential is available for this model; sign in through the management API or add an auth file",
			http.StatusUnauthorized,
		)
	}
	permit, errPermit := acquireSessionPermit(cfg, req, cred)
	if errPermit != nil {
		return sessionAdmissionFailure(errPermit)
	}
	defer permit.release()
	if cred.valid() {
		refreshed, errRefresh := prepareCredentialForUse(req.AuthID, cred)
		if errRefresh != nil {
			logWarn("expired credential could not be refreshed before chat", map[string]any{"error": errRefresh.Error()})
			return failEnvelope(
				"credential_refresh_failed",
				"the CodeArts credential expired and silent refresh failed; retry shortly or sign in again",
				http.StatusServiceUnavailable,
			)
		}
		cred = refreshed
	}

	chatSession, rejected, errSession := beginChatSession(cfg, req, cred)
	if errSession != nil {
		return failEnvelope("upstream_unreachable", "could not start the CodeArts chat session", http.StatusBadGateway)
	}
	if rejected != nil {
		return upstreamHTTPErrorEnvelope(rejected, cred)
	}
	if chatSession != nil {
		req.ChatSessionID = chatSession.ID()
		defer chatSession.Stop()
	}
	upstreamBody, endpoint, headers, errBuild := buildUpstreamRequest(cfg, req, cred, true)
	if errBuild != nil {
		return failEnvelope("invalid_request", errBuild.Error(), http.StatusBadRequest)
	}

	response, errDo := readUpstreamResponse(cfg, req.HostCallbackID, endpoint, headers, upstreamBody)
	if errDo != nil {
		return failEnvelope("upstream_unreachable", "upstream request failed: "+errDo.Error(), http.StatusBadGateway)
	}
	if response.StatusCode != http.StatusOK {
		return upstreamHTTPErrorEnvelope(response, cred)
	}

	payload, errAggregate := aggregateUpstream(cfg, req.Model, response.Body)
	if errAggregate != nil {
		return upstreamFailureEnvelope(errAggregate, cred)
	}
	// The host forwards the payload unchanged for Anthropic clients because the
	// plugin declares "claude" as an output format, so the plugin owns the
	// conversion there.
	if clientProtocol(req.Format) == protocolClaude {
		anthropic, errConvert := anthropicMessageFromCompletion(payload)
		if errConvert != nil {
			return failEnvelope("upstream_error", errConvert.Error(), http.StatusBadGateway)
		}
		payload = anthropic
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// executorExecuteStream handles the streaming path. When the host supplies a
// stream id the plugin emits chunks as they arrive; otherwise it falls back to
// buffering the whole response into the single synchronous reply.
func executorExecuteStream(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := config()
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return nil, fmt.Errorf("decode stored credential: %w", errCred)
	}
	if !cred.valid() && !cfg.InsistMissingCredentials {
		return failEnvelope("missing_credential", "no CodeArts Doer credential is available for this model", http.StatusUnauthorized)
	}
	permit, errPermit := acquireSessionPermit(cfg, req, cred)
	if errPermit != nil {
		return sessionAdmissionFailure(errPermit)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			permit.release()
		}
	}()
	if cred.valid() {
		refreshed, errRefresh := prepareCredentialForUse(req.AuthID, cred)
		if errRefresh != nil {
			logWarn("expired credential could not be refreshed before streaming chat", map[string]any{"error": errRefresh.Error()})
			return failEnvelope(
				"credential_refresh_failed",
				"the CodeArts credential expired and silent refresh failed; retry shortly or sign in again",
				http.StatusServiceUnavailable,
			)
		}
		cred = refreshed
	}

	chatSession, rejected, errSession := beginChatSession(cfg, req, cred)
	if errSession != nil {
		return failEnvelope("upstream_unreachable", "could not start the CodeArts chat session", http.StatusBadGateway)
	}
	if rejected != nil {
		return upstreamHTTPErrorEnvelope(rejected, cred)
	}
	if chatSession != nil {
		req.ChatSessionID = chatSession.ID()
		defer func() {
			if !handedOff {
				chatSession.Stop()
			}
		}()
	}
	upstreamBody, endpoint, headers, errBuild := buildUpstreamRequest(cfg, req, cred, true)
	if errBuild != nil {
		return failEnvelope("invalid_request", errBuild.Error(), http.StatusBadRequest)
	}

	// Without a stream id the host cannot receive asynchronous chunks, so the
	// response is buffered and returned inline.
	if strings.TrimSpace(req.StreamID) == "" {
		response, errDo := readUpstreamResponse(cfg, req.HostCallbackID, endpoint, headers, upstreamBody)
		if errDo != nil {
			return nil, fmt.Errorf("upstream request failed: %w", errDo)
		}
		if response.StatusCode != http.StatusOK {
			return upstreamHTTPErrorEnvelope(response, cred)
		}
		return bufferedStreamResponse(cfg, req.Model, clientProtocol(req.Format), response.Body, cred)
	}

	// Open before accepting the stream so CPA can observe 401/403/429 and
	// perform its normal account cooldown/retry logic with the real status.
	open, errOpen := hostHTTPDoStream(req.HostCallbackID, http.MethodPost, endpoint, headers, upstreamBody)
	if errOpen != nil {
		return failEnvelope("upstream_unreachable", errOpen.Error(), http.StatusBadGateway)
	}
	if open.StatusCode != http.StatusOK {
		return upstreamHTTPErrorEnvelope(readUpstreamErrorResponse(cfg, open), cred)
	}
	stop, errRegister := registerActiveStream(req.StreamID, func() {
		defer permit.release()
		_ = hostHTTPStreamClose(open.StreamID)
		if chatSession != nil {
			chatSession.Stop()
		}
	})
	if errRegister != nil {
		_ = hostHTTPStreamClose(open.StreamID)
		return nil, errRegister
	}
	session := &executorStreamSession{streamID: req.StreamID, stop: stop}
	gate := newStreamGate()
	deadline := time.Now().Add(cfg.requestTimeout())
	go func() {
		defer session.stop()
		runUpstreamStreamUntil(cfg, req, open, session, gate, deadline, cred)
	}()
	// Hand the stream over only once it is known to be answering. An upstream
	// envelope that arrives before any content is still reportable to the host as
	// a failed request, which is what lets it cool this account and try the next
	// one; after the first chunk reaches the client it no longer is.
	fault := gate.await(time.Until(deadline))
	// The pump owns the upstream handle from here on, including the abort path,
	// so its deferred stop is the single cleanup.
	handedOff = true
	if fault != nil {
		stop()
		return failEnvelope(fault.Code, redactUpstreamError(fault.Message, cred, false), fault.Status)
	}

	// An empty chunk list tells the host to consume the async stream bridge.
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// streamGate passes the executor's hand-off decision to the stream pump.
//
// CPA can only rotate to another credential when the plugin reports a status, and
// this gateway signals quota and rate-limit failures inside an HTTP 200 stream, so
// by the time those bytes are seen the response status is already committed. The
// gate lets the pump hold its frames until the stream has either produced answer
// traffic or failed before any answer, so a pre-answer envelope becomes an
// ordinary failed request instead of an empty success.
type streamGate struct {
	// head carries exactly one value: nil once the stream produced answer
	// traffic, or the fault that ended it before any answer arrived.
	head chan *upstreamStreamFault
	// verdict receives the executor's decision once: true means the client was
	// handed the stream and frames may be emitted, false means the executor has
	// already returned a failure and nothing may be emitted at all.
	verdict chan bool
}

func newStreamGate() *streamGate {
	return &streamGate{head: make(chan *upstreamStreamFault, 1), verdict: make(chan bool, 1)}
}

// reportHead tells the executor what the stream decided. It never blocks: when the
// executor stopped waiting first, the stream continues on the ordinary path and
// any later fault is reported through the session as before.
func (g *streamGate) reportHead(fault *upstreamStreamFault) {
	select {
	case g.head <- fault:
	default:
	}
}

// await returns a host-visible failure if the stream never answers by the deadline.
func (g *streamGate) await(wait time.Duration) *upstreamStreamFault {
	timeout := func() *upstreamStreamFault {
		return &upstreamStreamFault{Code: "upstream_timeout", Message: "upstream stream timed out before answering", Status: http.StatusGatewayTimeout}
	}
	if wait <= 0 {
		g.verdict <- false
		return timeout()
	}
	deadline := time.Now().Add(wait)
	var fault *upstreamStreamFault
	select {
	case fault = <-g.head:
	case <-time.After(wait):
		fault = timeout()
	}
	if fault == nil && !time.Now().Before(deadline) {
		fault = timeout()
	}
	g.verdict <- fault == nil
	return fault
}

// executorStreamSession owns the terminal transitions of one executor stream.
// A stream that times out while its reader is also failing must still produce
// exactly one error frame and one close: CPA treats a second close as an
// unrelated stream and a second error as a duplicate client-visible failure.
type executorStreamSession struct {
	streamID string
	stop     func()
	once     sync.Once
}

// fail reports a terminal error and ends the stream. The first caller wins.
func (s *executorStreamSession) fail(message string) {
	s.once.Do(func() {
		emitStreamError(s.streamID, message)
		s.stop()
		closeStream(s.streamID)
	})
}

// success ends the stream without reporting an error.
func (s *executorStreamSession) success() {
	s.once.Do(func() {
		s.stop()
		closeStream(s.streamID)
	})
}

// runUpstreamStream drives one upstream streaming request and forwards
// translated frames to the host. Every exit path goes through the session, so
// the upstream stream is closed exactly once. While the gate has not handed the
// stream over, the first decisive thing the pump sees decides whether the client
// gets a stream at all or a failed request the host can route around.
func runUpstreamStream(cfg *Config, req executorRequest, open *hostHTTPStreamOpen, session *executorStreamSession, gate *streamGate, credentials ...*credential) {
	runUpstreamStreamUntil(cfg, req, open, session, gate, time.Now().Add(cfg.requestTimeout()), credentials...)
}

func runUpstreamStreamUntil(cfg *Config, req executorRequest, open *hostHTTPStreamOpen, session *executorStreamSession, gate *streamGate, deadline time.Time, credentials ...*credential) {
	translator := newStreamRenderer(cfg, req.Model, clientProtocol(req.Format))
	var cred *credential
	if len(credentials) > 0 {
		cred = credentials[0]
	}
	released := gate == nil
	var pending [][]byte
	var pendingBytes int
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	armTimeout := func() {
		if gate != nil {
			timer = time.AfterFunc(time.Until(deadline), func() { session.fail("upstream request timed out") })
		}
	}
	// Hold only bounded pre-answer metadata. Anthropic can render message_start
	// for a role-only chunk, but the host must see no bytes until real content.
	queue := func(frames [][]byte) bool {
		for _, frame := range frames {
			pendingBytes += len(frame)
			if pendingBytes > 64<<10 {
				return false
			}
			pending = append(pending, frame)
		}
		return true
	}
	passHead := func() bool {
		gate.reportHead(nil)
		if !<-gate.verdict {
			return false
		}
		released = true
		armTimeout()
		return true
	}
	emit := func(frames [][]byte) bool {
		for _, frame := range frames {
			if errEmit := hostStreamEmit(session.streamID, translator.hostPayload(frame)); errEmit != nil {
				// Client cancellation closes the host bridge; there is no second
				// response to send after that point.
				session.success()
				return false
			}
		}
		return true
	}

	// reportFault ends a stream that produced no answer. Before the hand-off that
	// is still a status the host can act on; after it, the session carries the
	// error as the only remaining channel.
	reportFault := func(fault *upstreamStreamFault) {
		if released {
			session.fail(redactUpstreamError(fault.Message, cred, false))
			return
		}
		gate.reportHead(fault)
		if <-gate.verdict {
			session.fail(redactUpstreamError(fault.Message, cred, false))
		}
	}

	for {
		payload, done, errRead := hostHTTPStreamRead(open.StreamID)
		if errRead != nil {
			reportFault(&upstreamStreamFault{Code: "upstream_error", Status: http.StatusBadGateway,
				Message: "upstream stream read failed: " + errRead.Error()})
			return
		}
		if len(payload) > 0 {
			frames := translator.feed(payload)
			if !released {
				if !queue(frames) {
					reportFault(&upstreamStreamFault{Code: "upstream_error", Status: http.StatusBadGateway, Message: "upstream sent too much metadata before answering"})
					return
				}
				frames = nil
				if translator.hasAnswer() {
					if !passHead() {
						return
					}
					frames, pending = pending, nil
				}
			}
			if !emit(frames) {
				return
			}
		}
		// This gateway signals quota and rate-limit failures inside an HTTP 200
		// stream, so the envelope has to be checked while reading: at that point the
		// response status is still the plugin's to choose.
		if fault := translator.streamFault(); fault != nil {
			reportFault(fault)
			return
		}
		if done {
			break
		}
	}

	// An in-stream envelope means the answer never arrived, so the stream must not
	// be terminated as a success the client cannot distinguish from an empty
	// reply; failing it lets the host report and route around the condition.
	frames := translator.finish()
	if fault := translator.streamFault(); fault != nil {
		reportFault(fault)
		return
	}
	if !released {
		if !translator.hasAnswer() {
			reportFault(&upstreamStreamFault{Code: "upstream_empty_response", Status: http.StatusBadGateway, Message: "upstream stream ended without an answer"})
			return
		}
		if !queue(frames) {
			reportFault(&upstreamStreamFault{Code: "upstream_error", Status: http.StatusBadGateway, Message: "upstream sent too much metadata before answering"})
			return
		}
		if !passHead() {
			return
		}
		frames = pending
	}
	if !emit(frames) {
		return
	}
	session.success()
}

// bufferedStreamResponse renders the whole upstream response as one SSE reply
// for clients whose host connection cannot consume an async stream bridge.
func bufferedStreamResponse(cfg *Config, model, protocol string, body []byte, credentials ...*credential) ([]byte, error) {
	renderer := newStreamRenderer(cfg, model, protocol)
	frames := renderer.feed(body)
	frames = append(frames, renderer.finish()...)
	if fault := renderer.streamFault(); fault != nil {
		var cred *credential
		if len(credentials) > 0 {
			cred = credentials[0]
		}
		return upstreamFailureEnvelope(fault, cred)
	}

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, len(frames))
	for _, frame := range frames {
		if payload := renderer.hostPayload(frame); len(payload) > 0 {
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
		}
	}
	// The host decodes this result into rpcExecutorStreamResponse, which is
	// tagged in snake_case.
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
		"chunks":  chunks,
	})
}

// executorCountTokens estimates model-visible input from one request body.
// It does not call the upstream, allocate a chat session or consume quota.
func executorCountTokens(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	estimate := countInputTokens(req)
	// Anthropic clients read input_tokens from the count response and the host
	// forwards it unchanged for the declared claude output format.
	result := map[string]any{
		"total_tokens": estimate.Tokens,
		"estimated":    true,
		"note":         "local input estimate; model-specific tokenization and hidden prompt overhead may differ",
	}
	if len(estimate.Warnings) > 0 {
		result["warnings"] = estimate.Warnings
	}
	if clientProtocol(req.Format) == protocolClaude {
		result["input_tokens"] = estimate.Tokens
	}
	payload, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// executorHTTPRequest is not supported: the CodeArts Doer gateway is not an
// OpenAI-compatible passthrough, and silently proxying arbitrary paths would
// hide that from the caller.
func executorHTTPRequest(_ []byte) ([]byte, error) {
	return failEnvelope(
		"unsupported",
		"the CodeArts Doer plugin does not implement raw HTTP proxying; use the executor routes instead",
		http.StatusNotImplemented,
	)
}

// ---------------------------------------------------------------------------
// request construction
// ---------------------------------------------------------------------------

// buildUpstreamRequest renders the upstream body, signs it and returns the
// endpoint plus the complete header set to send.
func buildUpstreamRequest(cfg *Config, req executorRequest, cred *credential, stream bool) ([]byte, string, map[string]string, error) {
	req.Model = requestedModel(req)
	endpoint := strings.TrimRight(cfg.BaseURL, "/")
	var body []byte
	var errBuild error

	switch cfg.APIMode {
	case "native":
		endpoint += nativeChatPath
		body, errBuild = buildNativeBody(cfg, req, stream)
	default:
		endpoint += agentModePath
		body, errBuild = buildAgentBody(cfg, req, stream)
	}
	if errBuild != nil {
		return nil, "", nil, errBuild
	}

	headers := baseUpstreamHeaders(cfg, req)
	modelID := ""
	configuredBenefit := false
	if cfg.APIMode != "native" {
		modelID = cfg.upstreamModel(req.Model)
		configuredBenefit = cfg.isConfiguredBenefitModel(req.Model)
		for key := range headers {
			if strings.EqualFold(key, "model-id") || strings.EqualFold(key, "model-name") || strings.EqualFold(key, "x-model-id") {
				delete(headers, key)
			}
		}
		headers["model-id"] = modelID
		headers["model-name"] = modelID
		headers["x-model-id"] = modelID
		if configuredBenefit {
			for key := range headers {
				if strings.EqualFold(key, "maas_type") {
					delete(headers, key)
				}
			}
			headers["maas_type"] = "benefit"
		}
	}
	if cfg.APIMode != "native" && cfg.DiscoverModels && cred.valid() && !configuredBenefit {
		// Request routing must not start the optional benefit catalogue. A slow
		// catalogue lookup would delay an otherwise valid Agent Center chat and,
		// on older hosts, could outlive the executor callback. Explicit
		// benefit_models are merged without I/O by this path.
		catalog := accountAgentModelCatalog(cfg, cred, req.HostCallbackID)
		var selected *ModelConfig
		for i := range catalog.Models {
			if catalog.Models[i].ID == modelID {
				selected = &catalog.Models[i]
				break
			}
		}
		if selected == nil && catalog.Source == "configured" {
			for i := range catalog.Models {
				if catalog.Models[i].ID == req.Model {
					selected = &catalog.Models[i]
					break
				}
			}
		}
		if selected == nil {
			return nil, "", nil, fmt.Errorf("model %q is not available in this account's catalog; check the CodeArts panel model discovery results", modelID)
		}
		if catalog.Source == "discovered" {
			for key := range headers {
				if strings.EqualFold(key, "maas_type") {
					delete(headers, key)
				}
			}
		}
		if selected.Source == "benefit" {
			headers["maas_type"] = "benefit"
		}
	}
	if !cred.valid() {
		// Debugging escape hatch: send the request unsigned.
		logWarn("sending an unsigned upstream request because no credential is available", map[string]any{"endpoint": endpoint})
		return body, endpoint, headers, nil
	}
	signed, errSign := signRequest(http.MethodPost, endpoint, headers, body, cred, cfg.SignHost)
	if errSign != nil {
		return nil, "", nil, fmt.Errorf("sign upstream request: %w", errSign)
	}
	return body, endpoint, signed, nil
}

// baseUpstreamHeaders reproduces the protocol headers the official extension
// sends. These participate in the request signature.
func baseUpstreamHeaders(cfg *Config, req executorRequest) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/event-stream",
		"client_version": firstNonEmptyString(
			cfg.ClientVersion,
			"Vscode_"+cfg.PluginVersion,
		),
		"Agent-Type":      "ChatAgent",
		"X-Language":      cfg.Language,
		"x-snap-traceid":  strings.ReplaceAll(randomUUIDv4(), "-", ""),
		"plugin-name":     cfg.PluginName,
		"plugin-version":  cfg.PluginVersion,
		"is_confidential": fmt.Sprintf("%t", cfg.IsConfidential),
	}
	if cfg.Heartbeat {
		headers["heartbeat-enable"] = "true"
	}
	for key, value := range cfg.ExtraHeaders {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		headers[trimmed] = value
	}
	if req.ChatSessionID != "" {
		for key := range headers {
			if strings.EqualFold(key, "user-session-id") {
				delete(headers, key)
			}
		}
		headers["User-Session-Id"] = req.ChatSessionID
	}
	// Some deployments expect the client-authorised model to travel as a
	// header as well.
	if model := strings.TrimSpace(req.Model); model != "" {
		headers["x-model-id"] = model
	}
	return headers
}

// buildAgentBody renders an OpenAI-compatible request for /api/v2/chat/completions.
func buildAgentBody(cfg *Config, req executorRequest, stream bool) ([]byte, error) {
	payload := map[string]any{}
	if len(req.Payload) > 0 {
		if errUnmarshal := json.Unmarshal(req.Payload, &payload); errUnmarshal != nil {
			return nil, fmt.Errorf("client request body is not valid JSON: %w", errUnmarshal)
		}
	}
	model := cfg.upstreamModel(requestedModel(req))
	if model == "" {
		return nil, fmt.Errorf("model is required; select an ID from /v1/models")
	}
	payload["model"] = model
	payload["stream"] = stream
	if _, ok := payload["messages"]; !ok {
		return nil, fmt.Errorf("client request body has no messages array")
	}
	if stream {
		// The upstream includes a final usage frame when asked.
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(payload)
}

// requestedModel preserves the client-selected ID when a caller supplies it in
// the JSON payload instead of the host's separate Model field.
func requestedModel(req executorRequest) string {
	if model := strings.TrimSpace(req.Model); model != "" {
		return model
	}
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(req.Payload, &payload)
	return strings.TrimSpace(payload.Model)
}

// nativeMessage is one CodeArts context block. The native protocol models user
// input as typed blocks rather than role/content pairs.
type nativeMessage struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// buildNativeBody renders the proprietary /v1/chat/chat request.
func buildNativeBody(cfg *Config, req executorRequest, stream bool) ([]byte, error) {
	// This endpoint has no verified OpenAI tool/image request contract. Never
	// silently discard these fields: the agent endpoint preserves them intact.
	var original map[string]json.RawMessage
	if err := json.Unmarshal(req.Payload, &original); err != nil {
		return nil, err
	}
	for _, field := range []string{"tools", "tool_choice", "functions", "function_call"} {
		if raw := original[field]; len(raw) > 0 && string(raw) != "null" && string(raw) != "[]" {
			return nil, fmt.Errorf("native mode does not support %s; use api_mode: agent", field)
		}
	}
	var payload struct {
		Messages []struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			ToolCalls    json.RawMessage `json:"tool_calls"`
			FunctionCall json.RawMessage `json:"function_call"`
		} `json:"messages"`
		User     string         `json:"user"`
		Metadata map[string]any `json:"metadata"`
	}
	if len(req.Payload) > 0 {
		if errUnmarshal := json.Unmarshal(req.Payload, &payload); errUnmarshal != nil {
			return nil, fmt.Errorf("client request body is not valid JSON: %w", errUnmarshal)
		}
	}
	if len(payload.Messages) == 0 {
		return nil, fmt.Errorf("client request body has no messages array")
	}

	messages := make([]nativeMessage, 0, len(payload.Messages))
	for _, message := range payload.Messages {
		if message.Role == "tool" || message.Role == "function" || (len(message.ToolCalls) > 0 && string(message.ToolCalls) != "null" && string(message.ToolCalls) != "[]") || (len(message.FunctionCall) > 0 && string(message.FunctionCall) != "null") {
			return nil, fmt.Errorf("native mode does not support tool conversations; use api_mode: agent")
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Content, &parts) == nil {
			for _, part := range parts {
				if part.Type != "text" {
					return nil, fmt.Errorf("native mode only supports text; use api_mode: agent for multimodal input")
				}
			}
		}
		content := decodeMessageContent(message.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		// The native protocol has a single user-side context stream, so the role
		// is folded into the block text to preserve conversational structure.
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system":
			messages = append(messages, nativeMessage{Type: "text", Content: "System instruction:\n" + content})
		case "assistant":
			messages = append(messages, nativeMessage{Type: "text", Content: "Assistant:\n" + content})
		default:
			messages = append(messages, nativeMessage{Type: "text", Content: content})
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("client request body contains no usable message content")
	}

	modelID := cfg.upstreamModel(requestedModel(req))
	body := map[string]any{
		"chat_id":               strings.ReplaceAll(randomUUIDv4(), "-", ""),
		"messages":              messages,
		"client":                "IDE",
		"model_id":              modelID,
		"is_delta_response":     true,
		"batch_task_parameters": []any{},
		"task_parameters": map[string]any{
			"version":      "v1",
			"trigger_mode": "ENTER",
			"contexts":     []any{},
			"ide":          "CLIProxyAPI",
			"isNewClient":  true,
		},
	}
	if agentID := strings.TrimSpace(cfg.AgentID); agentID != "" {
		body["agent_id"] = agentID
	}
	if payload.User != "" {
		body["user_id"] = payload.User
	}
	return json.Marshal(body)
}

// decodeMessageContent accepts both plain string content and the array form
// used by multimodal OpenAI clients.
func decodeMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parts); errUnmarshal == nil {
		var builder strings.Builder
		for _, part := range parts {
			if part.Text != "" {
				if builder.Len() > 0 {
					builder.WriteString("\n")
				}
				builder.WriteString(part.Text)
			}
		}
		return builder.String()
	}
	return ""
}

// ---------------------------------------------------------------------------
// streaming renderer
// ---------------------------------------------------------------------------

// streamRenderer converts upstream bytes into frames for the client protocol.
type streamRenderer struct {
	mode       string
	protocol   string
	translator *nativeTranslator
	anthropic  *anthropicStreamRenderer
	decoder    sseDecoder
	done       bool
	finished   bool
	answered   bool
	// fault records the first in-stream error envelope so the caller can fail the
	// stream instead of closing it as a success with no content.
	fault *upstreamStreamFault
}

func (r *streamRenderer) hasAnswer() bool { return r.answered }

// streamFault returns the in-stream error envelope seen so far, if any.
func (r *streamRenderer) streamFault() *upstreamStreamFault {
	if r == nil {
		return nil
	}
	return r.fault
}

// observeFault records the first fault carried by a decoded frame and reports
// whether the frame must be withheld from the client.
func (r *streamRenderer) observeFault(payload []byte) bool {
	fault := streamFrameFault(payload)
	if fault == nil {
		return false
	}
	if r.fault == nil {
		r.fault = fault
	}
	return true
}

func newStreamRenderer(cfg *Config, model, protocol string) *streamRenderer {
	renderer := &streamRenderer{mode: cfg.APIMode, protocol: protocol}
	if cfg.APIMode == "native" {
		renderer.translator = newNativeTranslator(model)
	}
	if protocol == protocolClaude {
		renderer.anthropic = newAnthropicStreamRenderer(model)
	}
	return renderer
}

// feed consumes upstream bytes and returns frames to forward verbatim.
func (r *streamRenderer) feed(data []byte) [][]byte {
	if r.finished || r.fault != nil {
		return nil
	}
	// One decoder and fault check own every upstream mode, including native
	// frames destined for Anthropic. Never translate an error into empty success.
	var out [][]byte
	for _, frame := range r.decoder.push(data) {
		payload := executorStreamPayload([]byte(frame))
		if r.observeFault(payload) {
			break
		}
		if r.translator != nil {
			if chunk, ok := parseFrame(frame); ok {
				translated := r.translator.translateChunk(chunk)
				for _, part := range translated {
					r.answered = r.answered || streamFrameHasAnswer(executorStreamPayload(part))
				}
				out = append(out, r.renderOpenAI(translated)...)
			}
			continue
		}
		r.answered = r.answered || streamFrameHasAnswer(payload)
		if isDoneFrame([]byte(frame)) {
			if r.done {
				continue
			}
			r.done = true
		}
		out = append(out, r.renderOpenAI([][]byte{[]byte(frame + "\n\n")})...)
	}
	return out
}

// Output frames such as role-only deltas, usage and [DONE] are not answers.
func streamFrameHasAnswer(payload []byte) bool {
	var frame struct {
		Delta   json.RawMessage `json:"delta"`
		Text    string          `json:"text"`
		Choices []struct {
			Delta   json.RawMessage `json:"delta"`
			Message json.RawMessage `json:"message"`
			Text    string          `json:"text"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return false
	}
	if streamDeltaHasContent(frame.Delta) || (frame.Text != "" && frame.Text != doneSentinel) {
		return true
	}
	for _, choice := range frame.Choices {
		if streamDeltaHasContent(choice.Delta) || streamDeltaHasContent(choice.Message) || choice.Text != "" {
			return true
		}
	}
	return false
}

func (r *streamRenderer) renderOpenAI(frames [][]byte) [][]byte {
	if r.anthropic == nil {
		return frames
	}
	var out [][]byte
	for _, frame := range frames {
		if payload := executorStreamPayload(frame); len(payload) > 0 {
			out = append(out, r.anthropic.feed(payload)...)
		}
	}
	return out
}

// finish emits the terminating frames for the stream.
func (r *streamRenderer) finish() [][]byte {
	if r.finished {
		return nil
	}
	// EOF flush goes through exactly the same fault check as complete frames.
	out := r.feed([]byte("\n\n"))
	r.finished = true
	if r.fault != nil {
		return out
	}
	if r.translator != nil {
		out = append(out, r.renderOpenAI(r.translator.finalFrames())...)
		r.done = true
	}
	if r.anthropic != nil {
		return append(out, r.anthropic.finish()...)
	}
	if !r.done {
		out = append(out, []byte("data: [DONE]\n\n"))
		r.done = true
	}
	return out
}

// hostPayload converts one renderer frame into the bytes the host expects on the
// stream bridge.
//
// CPA's Chat Completions handler adds its own `data:` framing, so those frames
// must be bare JSON; its Anthropic handler writes the frame bytes unchanged, so
// the Anthropic SSE events are forwarded as-is.
func (r *streamRenderer) hostPayload(frame []byte) []byte {
	if r.anthropic != nil {
		return frame
	}
	return executorStreamPayload(frame)
}

// aggregateUpstream folds a complete upstream SSE body into a single
// non-streaming OpenAI completion. The result is deliberately protocol-neutral:
// callers convert it to the client protocol afterwards.
// upstreamStreamFault is an error the gateway reports inside an HTTP 200 SSE
// body. CodeArts speaks a top-level {"error_code","error_msg"} envelope here
// rather than the OpenAI {"error"} object, so a stream can end with neither
// content nor failure: an exhausted daily benefit allowance arrives this way and
// would otherwise be served to the client as a successful empty completion.
type upstreamStreamFault struct {
	Code    string
	Message string
	Status  int
}

func (f *upstreamStreamFault) Error() string { return f.Message }

// streamSuccessCodes are the envelope values this gateway family uses to mean
// "no fault" on a frame.
var streamSuccessCodes = map[string]bool{"0": true, "0000": true, "success": true}

// streamFrameFault reports the fault carried by one decoded SSE data payload, or
// nil when the frame is ordinary traffic. A frame that also carries a completion
// is never treated as a fault, so a partial answer is never discarded.
func streamFrameFault(payload []byte) *upstreamStreamFault {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	var frame struct {
		ErrorCode string          `json:"error_code"`
		ErrorMsg  string          `json:"error_msg"`
		Delta     json.RawMessage `json:"delta"`
		Choices   json.RawMessage `json:"choices"`
		Text      string          `json:"text"`
		Details   []struct {
			ErrorCode string `json:"error_code"`
			ErrorMsg  string `json:"error_msg"`
		} `json:"details"`
	}
	if errUnmarshal := json.Unmarshal(payload, &frame); errUnmarshal != nil {
		return nil
	}
	code := strings.TrimSpace(frame.ErrorCode)
	if code == "" || streamSuccessCodes[strings.ToLower(code)] {
		return nil
	}
	var choices []struct {
		Delta   json.RawMessage `json:"delta"`
		Message json.RawMessage `json:"message"`
		Text    string          `json:"text"`
	}
	_ = json.Unmarshal(frame.Choices, &choices)
	hasContent := streamDeltaHasContent(frame.Delta)
	for _, choice := range choices {
		hasContent = hasContent || streamDeltaHasContent(choice.Delta) || streamDeltaHasContent(choice.Message) || choice.Text != ""
	}
	if hasContent || (strings.TrimSpace(frame.Text) != "" && strings.TrimSpace(frame.Text) != doneSentinel) {
		return nil
	}

	message := "upstream reported " + code
	if text := strings.TrimSpace(frame.ErrorMsg); text != "" {
		message += ": " + text
	}
	// The envelope's details are trace decorations (requestId, modelId, …) that
	// repeat the same code, so only a genuinely additional message is appended.
	for _, detail := range frame.Details {
		text := strings.TrimSpace(detail.ErrorMsg)
		if text == "" || strings.Contains(message, text) || strings.Contains(text, ":") {
			continue
		}
		message += " (" + text + ")"
		break
	}

	fault := &upstreamStreamFault{Code: "upstream_error", Message: message, Status: http.StatusBadGateway}
	lowered := strings.ToLower(message)
	switch {
	case strings.Contains(lowered, "insufficient quota"):
		// Mirrors the HTTP mapping, where 403 means insufficient_quota.
		fault.Code, fault.Status = "insufficient_quota", http.StatusForbidden
	case strings.Contains(code, "429") || strings.Contains(lowered, "rate limit") || strings.Contains(lowered, "too many"):
		fault.Code, fault.Status = "rate_limit_exceeded", http.StatusTooManyRequests
	}
	return fault
}

// Null/empty containers and role-only deltas are metadata, not a partial answer.
func streamDeltaHasContent(raw json.RawMessage) bool {
	var delta struct {
		Content          json.RawMessage   `json:"content"`
		ReasoningContent string            `json:"reasoning_content"`
		ToolCalls        []json.RawMessage `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &delta) != nil {
		return false
	}
	return decodeMessageContent(delta.Content) != "" || delta.ReasoningContent != "" || len(delta.ToolCalls) > 0
}

// upstreamFailureEnvelope maps an aggregation failure to a host-visible error,
// preserving the classification the gateway carried inside the stream so the
// host can cool the credential over rather than serving an empty success.
func upstreamFailureEnvelope(err error, cred *credential) ([]byte, error) {
	var fault *upstreamStreamFault
	if errors.As(err, &fault) {
		return failEnvelope(fault.Code, redactUpstreamError(fault.Message, cred, false), fault.Status)
	}
	return failEnvelope("upstream_error", err.Error(), http.StatusBadGateway)
}

func aggregateUpstream(cfg *Config, model string, body []byte) ([]byte, error) {
	renderer := newStreamRenderer(cfg, model, protocolOpenAI)
	frames := renderer.feed(body)
	frames = append(frames, renderer.finish()...)
	// The renderer withholds fault envelopes from the frame list, so the recorded
	// fault is the only place an aggregated reply can learn about it.
	if fault := renderer.streamFault(); fault != nil {
		return nil, fault
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("upstream returned an empty response")
	}
	// Agent mode finishes with a done marker that carries no content; dropping it
	// keeps the aggregator from seeing a sentinel frame.
	aggregator := newAggregator("", cfg.upstreamModel(model))
	for _, frame := range frames {
		var fault struct {
			Error json.RawMessage `json:"error"`
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(frame), []byte("data:")))
		if json.Unmarshal(data, &fault) == nil && len(fault.Error) > 0 && string(fault.Error) != "null" {
			return nil, fmt.Errorf("upstream returned an error frame: %s", truncate(string(fault.Error), 300))
		}
		aggregator.addFrame(frame)
	}
	return aggregator.completion(), nil
}

func decodeExecutorRequest(request []byte) (executorRequest, error) {
	var req executorRequest
	if len(request) == 0 {
		return req, fmt.Errorf("executor request is empty")
	}
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return req, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// stream lifecycle
// ---------------------------------------------------------------------------

// registerActiveStream records an in-flight executor stream so shutdown can
// cancel it, and returns a function that deregisters it.
func registerActiveStream(streamID string, cleanup ...func()) (func(), error) {
	var once sync.Once
	stop := func() {
		once.Do(func() {
			for _, f := range cleanup {
				f()
			}
			activeStreams.Delete(streamID)
		})
	}
	if _, loaded := activeStreams.LoadOrStore(streamID, stop); loaded {
		return nil, fmt.Errorf("stream %s is already active", streamID)
	}
	return stop, nil
}

func closeAllActiveStreams() {
	activeStreams.Range(func(key, value any) bool {
		if cancel, ok := value.(func()); ok {
			cancel()
		}
		if id, ok := key.(string); ok {
			closeStream(id)
		}
		activeStreams.Delete(key)
		return true
	})
}

// Buffer a host-owned stream for non-streaming clients, retaining request
// cancellation via callback ID and enforcing our configured read deadline.
func readUpstreamResponse(cfg *Config, callbackID, endpoint string, headers map[string]string, body []byte) (*hostHTTPResponse, error) {
	open, err := hostHTTPDoStream(callbackID, http.MethodPost, endpoint, headers, body)
	if err != nil {
		return nil, err
	}
	if open.StatusCode != http.StatusOK {
		return readUpstreamErrorResponse(cfg, open), nil
	}
	defer hostHTTPStreamClose(open.StreamID)
	resp := &hostHTTPResponse{StatusCode: open.StatusCode, Headers: open.Headers}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.requestTimeout())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = hostHTTPStreamClose(open.StreamID) })
	defer stop()
	for {
		payload, done, err := hostHTTPStreamRead(open.StreamID)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		if len(resp.Body)+len(payload) > 64<<20 {
			return nil, fmt.Errorf("upstream response exceeds 64 MiB")
		}
		resp.Body = append(resp.Body, payload...)
		if done {
			return resp, nil
		}
	}
}

const (
	upstreamErrorBodyLimit   = 8 << 10
	upstreamErrorChunkLimit  = 128
	upstreamErrorReadTimeout = 2 * time.Second
)

// Read diagnostic bytes from the already-open failed request. Error responses
// do not need the normal ten-minute generation deadline or an unbounded body.
// A slow/broken stream must never replace the upstream HTTP status with a 502.
func readUpstreamErrorResponse(cfg *Config, open *hostHTTPStreamOpen) *hostHTTPResponse {
	response := &hostHTTPResponse{StatusCode: open.StatusCode, Headers: open.Headers}
	defer hostHTTPStreamClose(open.StreamID)
	timeout := upstreamErrorReadTimeout
	if cfg != nil && cfg.requestTimeout() > 0 && cfg.requestTimeout() < timeout {
		timeout = cfg.requestTimeout()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	type readResult struct {
		payload []byte
		done    bool
		err     error
	}
	for reads := 0; reads < upstreamErrorChunkLimit; reads++ {
		// A single buffered result lets a pending read exit after timeout and
		// stream_close. Do not start another read until this one is consumed.
		result := make(chan readResult, 1)
		go func() {
			payload, done, err := hostHTTPStreamRead(open.StreamID)
			result <- readResult{payload: payload, done: done, err: err}
		}()
		select {
		case <-timer.C:
			response.ErrorReadNote = "error response read timed out"
			return response
		case chunk := <-result:
			remaining := upstreamErrorBodyLimit - len(response.Body)
			if len(chunk.payload) > remaining {
				response.Body = append(response.Body, chunk.payload[:remaining]...)
				response.ErrorReadNote = "error response truncated at 8 KiB"
				return response
			}
			response.Body = append(response.Body, chunk.payload...)
			if chunk.err != nil {
				// Host errors can contain raw transport details. The partial
				// upstream body is useful; a generic read note is sufficient.
				response.ErrorReadNote = "error response read failed"
				return response
			}
			if chunk.done {
				return response
			}
			if len(response.Body) == upstreamErrorBodyLimit {
				response.ErrorReadNote = "error response truncated at 8 KiB"
				return response
			}
		}
	}
	response.ErrorReadNote = "error response stopped after too many chunks"
	return response
}

func upstreamHTTPErrorEnvelope(response *hostHTTPResponse, cred *credential) ([]byte, error) {
	code := "upstream_error"
	switch response.StatusCode {
	case http.StatusUnauthorized:
		code = "invalid_credential"
	case http.StatusForbidden:
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		code = "rate_limit_exceeded"
	}
	message := fmt.Sprintf("upstream returned HTTP %d", response.StatusCode)
	detail := redactUpstreamError(string(response.Body), cred, response.ErrorReadNote != "")
	if detail = strings.TrimSpace(detail); detail != "" {
		message += ": " + truncate(detail, upstreamErrorBodyLimit)
	}
	if response.ErrorReadNote != "" {
		message += " (" + response.ErrorReadNote + ")"
	}
	return failEnvelope(code, message, httpStatusFor(response.StatusCode))
}

// Error bodies sometimes echo a rejected credential or request headers. Remove
// this account's secret values, including JSON/form-encoded spellings, before
// sending diagnostics to a downstream API client.
func redactUpstreamError(message string, cred *credential, partial bool) string {
	if cred == nil {
		return message
	}
	secrets := []string{cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken, cred.RefreshToken}
	if oauth := cred.OAuthContext; oauth != nil {
		secrets = append(secrets, oauth.PKCEPair.CodeVerifier, oauth.PKCEPair.CodeChallenge,
			oauth.DPoPKeyPair.PrivateKeyJWK.D, oauth.DPoPKeyPair.PrivateKeyJWK.X,
			oauth.DPoPKeyPair.PrivateKeyJWK.Y, oauth.DPoPKeyPair.PublicKeyJWK.X,
			oauth.DPoPKeyPair.PublicKeyJWK.Y)
	}
	variants := map[string]bool{}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		variants[secret] = true
		variants[url.QueryEscape(secret)] = true
		encoded, _ := json.Marshal(secret)
		variants[string(encoded[1:len(encoded)-1])] = true
	}
	secrets = secrets[:0]
	for secret := range variants {
		secrets = append(secrets, secret)
	}
	// Redact complete long values before their possible component substrings.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
		if !partial {
			continue
		}
		// The body bound or read failure can cut a credential in half. Also
		// redact any known credential prefix at that final byte boundary.
		for size := min(len(message), len(secret)-1); size > 0; size-- {
			if strings.HasSuffix(message, secret[:size]) {
				message = message[:len(message)-size] + "[REDACTED]"
				break
			}
		}
	}
	return message
}

// ---------------------------------------------------------------------------
// host callback wrappers
// ---------------------------------------------------------------------------

// hostHTTPResponse matches the host's buffered HTTP reply. The host marshals
// pluginapi.HTTPResponse, which has no JSON tags, so the field names are Go
// identifiers here.
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
	// ErrorReadNote is local diagnostic metadata, never part of the host wire.
	ErrorReadNote string `json:"-"`
}

// hostHTTPDo performs one buffered HTTP request through the host transport so
// proxy settings, logging and request capture stay under host policy.
func hostHTTPDo(method, endpoint string, headers map[string]string, body []byte) (*hostHTTPResponse, error) {
	return hostHTTPDoContext("", method, endpoint, headers, body)
}

func hostHTTPDoContext(callbackID, method, endpoint string, headers map[string]string, body []byte) (*hostHTTPResponse, error) {
	result, errCall := hostCall("host.http.do", map[string]any{
		"host_callback_id": callbackID,
		"method":           method,
		"url":              endpoint,
		"headers":          toHeaderMap(headers),
		"body":             body,
	})
	if errCall != nil {
		return nil, errCall
	}
	var response hostHTTPResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
	}
	return &response, nil
}

// hostHTTPStreamOpen matches the host's rpcHostHTTPStreamResponse, which is
// tagged in snake_case: encoding/json does not fold "status_code" onto a Go
// field named StatusCode, so the tags must match the host exactly.
type hostHTTPStreamOpen struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	StreamID   string              `json:"stream_id"`
}

// hostHTTPStreamChunk matches the host's rpcHostHTTPStreamReadResponse.
type hostHTTPStreamChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

// hostHTTPDoStream opens a streaming HTTP request through the host.
func hostHTTPDoStream(hostCallbackID, method, endpoint string, headers map[string]string, body []byte) (*hostHTTPStreamOpen, error) {
	request := map[string]any{
		"method":  method,
		"url":     endpoint,
		"headers": toHeaderMap(headers),
		"body":    body,
	}
	if strings.TrimSpace(hostCallbackID) != "" {
		// Forwarding the callback id keeps the nested execution scoped to this
		// plugin's request context.
		request["host_callback_id"] = hostCallbackID
	}
	result, errCall := hostCall("host.http.do_stream", request)
	if errCall != nil {
		return nil, errCall
	}
	var open hostHTTPStreamOpen
	if errUnmarshal := json.Unmarshal(result, &open); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream open: %w", errUnmarshal)
	}
	if open.StreamID == "" {
		return nil, fmt.Errorf("host returned no stream id")
	}
	return &open, nil
}

// hostHTTPStreamRead reads the next chunk of a host-brokered HTTP stream.
func hostHTTPStreamRead(streamID string) ([]byte, bool, error) {
	result, errCall := hostCall("host.http.stream_read", map[string]any{"stream_id": streamID})
	if errCall != nil {
		return nil, false, errCall
	}
	var chunk hostHTTPStreamChunk
	if errUnmarshal := json.Unmarshal(result, &chunk); errUnmarshal != nil {
		return nil, false, fmt.Errorf("decode host stream chunk: %w", errUnmarshal)
	}
	if chunk.Error != "" {
		return chunk.Payload, chunk.Done, fmt.Errorf("%s", chunk.Error)
	}
	return chunk.Payload, chunk.Done, nil
}

// hostHTTPStreamClose releases a host-brokered HTTP stream. Streaming callbacks
// must always be closed explicitly.
func hostHTTPStreamClose(streamID string) error {
	_, errCall := hostCall("host.http.stream_close", map[string]any{"stream_id": streamID})
	return errCall
}

// hostStreamEmit pushes one prepared frame to the client. The caller decides the
// framing through streamRenderer.hostPayload: CPA's Chat Completions handler adds
// its own `data:` wrapper and `[DONE]`, while the Anthropic handler writes the
// frame unchanged.
func hostStreamEmit(streamID string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, errCall := hostCall("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	return errCall
}

// executorStreamPayload reduces a client-protocol SSE frame to the payload CPA's
// Chat Completions handler expects: bare JSON, no `data:` prefix, no [DONE]
// sentinel and no comment frames. Sending framed SSE there double-wraps it.
func executorStreamPayload(frame []byte) []byte {
	frame = bytes.TrimSpace(frame)
	if bytes.HasPrefix(frame, []byte(":")) {
		return nil
	}
	frame = bytes.TrimSpace(bytes.TrimPrefix(frame, []byte("data:")))
	if bytes.Equal(frame, []byte(doneSentinel)) {
		return nil
	}
	return frame
}

func emitStreamError(streamID, message string) {
	_, errCall := hostCall("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"error":     message,
	})
	if errCall != nil {
		logWarn("failed to report a stream error to the host", map[string]any{"error": message})
	}
}

// closeStream signals the end of an executor stream.
func closeStream(streamID string) {
	if _, errCall := hostCall("host.stream.close", map[string]any{"stream_id": streamID}); errCall != nil {
		logWarn("failed to close the host stream", map[string]any{"stream_id": streamID})
	}
}

// toHeaderMap converts a flat header map into the multi-value form the host
// expects.
func toHeaderMap(headers map[string]string) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, value := range headers {
		out[key] = []string{value}
	}
	return out
}
