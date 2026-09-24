## Capability coverage

CLIProxyAPI defines 26 plugin extension points. This plugin implements the ones
that are meaningful for a CodeArts Doer provider and deliberately leaves the rest
undeclared, because the host trusts a declaration: claiming a capability routes
real traffic through code that may not handle it.

### Implemented (declared in `plugin.register`)

| Capability | Status | What it does here |
| --- | --- | --- |
| `auth_provider` | yes | OAuth authorization-code sign-in with PKCE + DPoP, hourly host/cron renewal, request-time expiry catch-up, and legacy ticket compatibility. |
| `model_provider` | yes | Discovers account agents and their visible models on CPA's critical path. Benefit models come from operator-confirmed `benefit_models` or an explicit synchronous management refresh, so a slow optional gateway cannot empty the registry. Configured aliases retain the target route; no model is fabricated on Agent discovery failure. |
| `executor` | yes | Chat completions, streaming and non-streaming, both upstream protocols. Declares input `chat-completions` and outputs `chat-completions` + `claude`, so OpenAI, Anthropic and Responses clients are served (see below). |
| `quota_provider` | yes | Subscription/quota view in the host's normalised quota shape. |
| `usage_plugin` | yes | Collects the host's exact per-request token records into a rollup. |
| `thinking_applier` | yes | Maps validated thinking config onto `reasoning_effort`. |
| `scheduler` | yes | Rotates across accounts so per-account rate limits are not hit. |
| `management_api` | yes | REST routes plus a browser dashboard. |

### Declared executor formats

```json
"executor_input_formats":  ["chat-completions"],
"executor_output_formats": ["chat-completions", "claude"]
```

The host uses the declaration twice: to translate the inbound request into a
declared input format, and to decide who frames the response. A declared client
protocol is passed through untouched; an undeclared one is translated by the
host. The two client protocols need opposite framing from the same plugin — the
Chat Completions handler writes its own `data:` wrapper, while the host's
OpenAI→Claude stream translator only accepts frames that already carry one — and
`ExecutorRequest` does not expose the original client protocol. Declaring
`claude` makes the host select and pass it through, which is the only way the
plugin can tell the two apart (`ExecutorRequest.Format`), and the plugin then
renders Anthropic SSE itself (see `claude_output.go`).

`responses` is deliberately *not* declared: the host's OpenAI→Responses
translator accepts the bare JSON chunks the plugin emits, so declaring it would
only take that translation away from the host for no benefit.

### Not implemented (left undeclared, with reasons)

| Capability | Why not |
| --- | --- |
| `model_registrar` | `model_provider` already contributes static models; declaring both would register the list twice. |
| `frontend_auth_provider` / `_exclusive` | These authenticate *incoming client* requests to the gateway. That is the gateway operator's concern and belongs to client API keys, not to an upstream provider plugin. The exclusive variant would take over the gateway's entire request authentication. |
| `model_router` | Reserved for plugins that must redirect a request *before* provider selection (for example routing Claude Code's built-in web search to another backend). CodeArts Doer requests resolve normally, so there is nothing to redirect. |
| `request_translator` / `response_translator` | These handle canonical-to-provider protocol conversion. The executor already speaks OpenAI chat-completions directly, so a separate translation stage would be a pointless round trip. |
| `request_normalizer` | Would rewrite *inbound* requests before execution. The executor already builds the upstream body (`buildAgentBody` / `buildNativeBody`), so normalising twice would duplicate that logic. |
| `response_before_translator` / `response_after_translator` | Response normalisation around native translation. Streaming conversion already happens inside the executor's `streamRenderer`, and the `native` protocol is not a documented transform target. |
| `request_interceptor` / `response_interceptor` / `response_stream_interceptor` | Generic rewriting hooks. This plugin has no rewriting policy to apply; a deployment that wants one should use a dedicated interceptor plugin (for example the official `jshandler`), which composes with this provider. |
| `request_lifecycle_plugin` | Observes terminal request events. `usage_plugin` already receives the completed record with latency, TTFT and failure details, so this would be a second subscription to the same signal. |
| `websocket_response_observer` | CodeArts Doer has no WebSocket upstream transport; both supported paths are HTTP + SSE. |
| `command_line_plugin` | Would add plugin-specific CLI flags to the gateway binary. There is nothing to configure at that level here: every knob lives in `plugins.configs.codearts-provider`. |

A documentation gap worth knowing: the published development docs do not list
`quota_provider`, `request_lifecycle_plugin` or `websocket_response_observer`, but
the v7.3.4 SDK and host do implement them. This plugin uses `quota_provider` and
leaves the other two undeclared as explained above.

### Adaptation notes

Three things were needed to fit the host's model rather than fight it:

1. **The general scheduler is self-driven.** Arbitrary `schedule` tasks use
   `robfig/cron` inside the plugin. Credential renewal additionally participates
   in the host auth refresh loop and has a request-time catch-up path.
2. **The executor always streams upstream.** The CodeArts native protocol has no
   non-streaming variant, so `executor.execute` buffers and aggregates a stream
   internally. From the host's perspective it is an ordinary non-streaming
   executor.
3. **Anthropic output is rendered by the plugin.** The `claude` output format is
   declared precisely so the host does *not* translate it; the trade-off is that
   the plugin owns the Anthropic event sequence and message body, which
   `claude_output.go` implements by mirroring the host's own OpenAI→Claude
   translator.

