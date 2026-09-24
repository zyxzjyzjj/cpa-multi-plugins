// Command codearts-provider builds a CLIProxyAPI native dynamic-library plugin that
// exposes the Huawei CodeArts Doer (CodeArts Agent / CodeBot) upstream models
// through the CLIProxyAPI plugin C ABI.
//
// The plugin is a *standard dynamic library plugin*: CLIProxyAPI dlopen()s the
// produced .dll/.so/.dylib, calls cliproxy_plugin_init, and from then on talks
// to the plugin exclusively through the JSON-over-C-ABI `call` function table.
//
// Build (CGO is mandatory, the plugin ABI is a C ABI):
//
//	CGO_ENABLED=1 go build -buildmode=c-shared -o codearts-provider.dll .
//
// See README.md for the full install and configuration walkthrough.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* codeartsProviderHostApi;

static void store_host_api(const cliproxy_host_api* host) {
	codeartsProviderHostApi = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (codeartsProviderHostApi == NULL || codeartsProviderHostApi->call == NULL) {
		return 1;
	}
	return codeartsProviderHostApi->call(codeartsProviderHostApi->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (codeartsProviderHostApi != NULL && codeartsProviderHostApi->free_buffer != NULL && ptr != NULL) {
		codeartsProviderHostApi->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// providerID is both the executor/auth provider identifier and the plugin ID
// convention used for model attribution. It must be lowercase because the host
// normalizes provider identifiers.
const providerID = "codearts-provider"

var version = "0.12.89" // overridden by the unified release build

var (
	// currentConfig holds the last configuration delivered by the host. It is
	// replaced wholesale by plugin.register / plugin.reconfigure.
	currentConfig atomic.Pointer[Config]

	// shutdownOnce guards idempotent plugin shutdown.
	shutdownOnce sync.Once

	// logSink forwards plugin diagnostics to the host logger. It is installed on
	// the first register/reconfigure call and depends on the host API pointer.
	logSink *hostLogger
)

// envelope is the JSON RPC envelope exchanged across the C ABI. It matches
// pluginabi.Envelope, including the optional http_status on errors.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	logSink = newHostLogger()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), 0))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// This export runs during host teardown, immediately before the library is
	// released. Touching Go runtime state (channels, mutexes, goroutine
	// synchronization) from here risks a crash while the host is unwinding, so
	// background work is stopped earlier, from plugin.quiesce instead. The
	// reference implementation documents the same hazard.
	shutdownOnce.Do(func() {})
}

// handleMethod dispatches one host RPC call and returns the marshalled envelope.
func handleMethod(method string, request []byte) ([]byte, error) {
	reconfigure := method == pluginabi.MethodPluginReconfigure

	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		cfg, errConfig := parseConfig(request)
		if errConfig != nil {
			return nil, fmt.Errorf("parse plugin config: %w", errConfig)
		}
		// Scheduled work is owned by the plugin: the host ABI has no timer, so
		// the cron runner is (re)built here on every register/reconfigure.
		installScheduledConfig(cfg)
		if logSink != nil {
			if reconfigure {
				logSink.info("plugin reconfigured", map[string]any{"base_url": cfg.BaseURL})
			} else {
				logSink.info("plugin registered", map[string]any{
					"base_url": cfg.BaseURL,
					"api_mode": cfg.APIMode,
					"models":   len(cfg.Models),
				})
			}
		}
		return okEnvelope(registrationResponse())

	case pluginabi.MethodPluginQuiesce:
		// Quiesce runs while the host runtime is still healthy, so it is the
		// safe place to stop background work.
		stopScheduleSettings()
		shutdownScheduler()
		stopLoginSessions()
		closeAllActiveStreams()
		stopAllChatSessions()
		return okEnvelope(map[string]any{})

	case pluginabi.MethodPluginShutdown:
		stopLoginSessions()
		closeAllActiveStreams()
		stopAllChatSessions()
		return okEnvelope(map[string]any{})

	case pluginabi.MethodModelRegister:
		return okEnvelope(modelRegistration())
	case pluginabi.MethodModelStatic:
		return okEnvelope(staticModels())
	case pluginabi.MethodModelForAuth:
		return modelsForAuth(request)
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": providerID})
	case pluginabi.MethodAuthParse:
		return authParse(request)
	case pluginabi.MethodAuthLoginStart:
		return authLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return authLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return authRefresh(request)
	case pluginabi.MethodExecutorExecute:
		return executorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return executorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return executorHTTPRequest(request)
	case pluginabi.MethodSchedulerPick:
		return schedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return usageHandle(request)
	case pluginabi.MethodThinkingIdentifier:
		return okEnvelope(map[string]string{"identifier": providerID})
	case pluginabi.MethodThinkingApply:
		return thinkingApply(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": providerID})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(quotaDescribe())
	case pluginabi.MethodQuotaFetch:
		return quotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return quotaReset()
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return managementHandle(request)
	default:
		return nil, fmt.Errorf("unsupported method %s", method)
	}
}

// ---------------------------------------------------------------------------
// host callbacks
// ---------------------------------------------------------------------------

// hostCall invokes one host callback and unwraps the JSON RPC envelope.
//
// The implementation lives behind an atomic slot rather than in a plain variable
// because plugin background goroutines (stream pumps, login pollers, scheduled
// tasks) call it for as long as they run, and callers such as tests replace the
// implementation while those goroutines may still be in flight.
type hostCallFunc func(string, any) (json.RawMessage, error)

var hostCallSlot atomic.Value

func hostCall(method string, request any) (json.RawMessage, error) {
	fn, _ := hostCallSlot.Load().(hostCallFunc)
	if fn == nil {
		fn = callHostABI
	}
	return fn(method, request)
}

// setHostCall installs a host callback implementation and returns a function
// that restores the previous one.
func setHostCall(fn hostCallFunc) func() {
	previous, _ := hostCallSlot.Load().(hostCallFunc)
	if previous == nil {
		previous = callHostABI
	}
	if fn == nil {
		fn = callHostABI
	}
	hostCallSlot.Store(fn)
	return func() { hostCallSlot.Store(previous) }
}

func callHostABI(method string, request any) (json.RawMessage, error) {
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host request %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var cRequest *C.uint8_t
	if len(payload) > 0 {
		cRequest = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(cRequest))
	}
	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, cRequest, C.size_t(len(payload)), &response)
	if response.ptr != nil {
		// The host allocated this buffer with the host allocator; it must be
		// released through the host free callback, never through C.free.
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if response.ptr == nil || response.len == 0 {
		return nil, nil
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host callback %s: %s", method, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s returned an error", method)
	}
	return env.Result, nil
}

// hostLogger forwards diagnostics to the host log through host.log.
type hostLogger struct {
	mu sync.Mutex
}

func newHostLogger() *hostLogger { return &hostLogger{} }

func (l *hostLogger) info(message string, fields map[string]any) {
	l.log("info", message, fields)
}

func (l *hostLogger) warn(message string, fields map[string]any) {
	l.log("warn", message, fields)
}

func (l *hostLogger) log(level, message string, fields map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = hostCall(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
}

func logInfo(message string, fields map[string]any) {
	if logSink != nil {
		logSink.info(message, fields)
	}
}

func logWarn(message string, fields map[string]any) {
	if logSink != nil {
		logSink.warn(message, fields)
	}
}

// ---------------------------------------------------------------------------
// envelope helpers
// ---------------------------------------------------------------------------

func okEnvelope(result any) ([]byte, error) {
	rawResult, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: rawResult})
}

// errorEnvelopeRaw builds a failed envelope. httpStatus is optional; when it is
// zero or omitted CLIProxyAPI reports HTTP 500 to the client.
func errorEnvelopeRaw(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus},
	})
	return raw
}

// errorEnvelope wraps pluginabi.NewErrorEnvelope semantics for the call path.
func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, errMarshal := pluginabi.NewErrorEnvelope(code, message, httpStatus)
	if errMarshal != nil {
		return errorEnvelopeRaw(code, message, httpStatus)
	}
	return raw
}

// failEnvelope returns a failed envelope plus the nil error expected by the
// dispatch contract (the envelope itself already carries the failure).
func failEnvelope(code, message string, httpStatus int) ([]byte, error) {
	return errorEnvelope(code, message, httpStatus), nil
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
