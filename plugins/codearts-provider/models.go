package main

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginRepositoryURL = "https://github.com/zyxzjyzjj/cpa-multi-plugins"

// registrationResponse advertises the plugin metadata and the capabilities the
// plugin actually implements. Only implemented capabilities are declared:
// CLIProxyAPI rejects or mis-routes a plugin that over-declares.
func registrationResponse() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "CodeArts",
			Version:          version,
			Author:           "converted from huaweicloud.vscode-codebot",
			GitHubRepository: pluginRepositoryURL,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "CodeArts Doer regional API base URL, for example https://snap-access.cn-north-4.myhuaweicloud.com.",
				},
				{
					Name:        "api_mode",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"agent", "native"},
					Description: "Upstream protocol: agent uses the OpenAI-compatible /api/v2/chat/completions endpoint, native uses /v1/chat/chat with CodeArts SSE framing.",
				},
				{
					Name:        "web_login_base",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "CodeArts web console base URL used to start the browser login flow.",
				},
				{
					Name:        "plugin_name",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Value sent as the plugin-name header. Defaults to snap_vscode.",
				},
				{
					Name:        "plugin_version",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Value sent as the plugin-version and client_version headers.",
				},
				{
					Name:        "language",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"zh-cn", "en-us"},
					Description: "Value sent as the X-Language header.",
				},
				{
					Name:        "agent_id",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "CodeArts agent UUID or alias used by the native protocol.",
				},
				{
					Name:        "discover_models",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Discover account models, including bounded initial benefit discovery and a persistent account-scoped benefit catalogue.",
				},
				{
					Name:        "discover_builtin_models",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Query the account-scoped /v1/model/builtin catalogue after Agent Center. Enabled by default; disable it if that endpoint is slow.",
				},
				{
					Name:        "benefit_gateway_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Benefit model catalog base, default https://opengw.developer.huaweicloud.com. Empty disables this catalog; chats use the CodeArts base_url.",
				},
				{
					Name: "discovery_proxy_url", Type: pluginapi.ConfigFieldTypeString,
					Description: "Optional proxy override for five-second initial benefit discovery; otherwise inherits the host proxy. direct:// disables proxies. Chat and management retain host transport policy.",
				},
				{
					Name:        "model_agent_ids",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Optional Agent Center catalog IDs. Empty discovers the account's agents automatically.",
				},
				{
					Name:        "default_model_id",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Optional upstream model ID for requests that omit a model. Empty by default.",
				},
				{
					Name:        "models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Static model list advertised to CLIProxyAPI.",
				},
				{
					Name:        "benefit_models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Operator-confirmed benefit models shared by all CodeArts accounts and routed with maas_type=benefit without blocking cold-start discovery.",
				},
				{
					Name:        "model_map",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Map of client-facing model IDs to upstream model IDs.",
				},
				{
					Name:        "schedule",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Cron-driven plugin tasks. The plugin ABI has no timer, so the plugin schedules its own work (token_renew, quota_refresh, or an arbitrary http request).",
				},
				{
					Name:        "sign_host",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Include host in regional API signatures. Benefit gateway requests always sign host independently of this setting.",
				},
				{
					Name:        "is_confidential",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Set the is_confidential header for KIA (confidential code) mode.",
				},
				{
					Name:        "heartbeat",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Request upstream SSE heartbeat comment frames.",
				},
				{
					Name:        "chat_session_heartbeat",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Report each chat session as busy/idle so completed requests release upstream concurrency slots. Enabled by default in agent mode.",
				},
				{
					Name:        "request_timeout_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Timeout in seconds for a single upstream request.",
				},
				{Name: "chat_session_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Description: "Default local concurrent chat limit per CodeArts account (1–64, default 3). Account panel overrides take precedence. Does not raise the upstream subscription limit."},
				{
					Name:        "insist_missing_credentials",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Send unsigned upstream requests when no credential is available. Debugging only.",
				},
				{
					Name:        "extra_headers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Additional headers added to every upstream request and included in the signature.",
				},
			},
		},
		"capabilities": declaredCapabilities(),
	}
}

// modelRegistration returns the models contributed to the registry.
func modelRegistration() pluginapi.ModelRegistrationResponse {
	return pluginapi.ModelRegistrationResponse{
		Provider: providerID,
		Models:   modelInfos(),
	}
}

// staticModels returns the provider-native static model list. The upstream model
// catalogue is account specific. Only explicit operator overrides are static;
// the default list is empty and model.for_auth supplies the discovered catalog.
func staticModels() pluginapi.ModelResponse {
	return pluginapi.ModelResponse{
		Provider: providerID,
		Models:   modelInfos(),
	}
}

func modelInfos() []pluginapi.ModelInfo {
	cfg := config()
	models := make([]ModelConfig, 0, len(cfg.Models)+len(cfg.BenefitModels))
	seen := make(map[string]bool, cap(models))
	for _, source := range [][]ModelConfig{cfg.Models, cfg.BenefitModels} {
		for _, model := range source {
			if model.ID == "" || seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			models = append(models, model)
		}
	}
	return infosForModels(models)
}

func infosForModels(models []ModelConfig) []pluginapi.ModelInfo {
	now := time.Now().Unix()
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		modalities := []string{"text"}
		if model.SupportsImages && config().APIMode == "agent" {
			modalities = append(modalities, "image")
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         model.ID,
			Object:                     "model",
			Created:                    now,
			OwnedBy:                    providerID,
			Type:                       "chat",
			DisplayName:                model.DisplayName,
			Name:                       model.Name,
			Description:                model.Description,
			InputTokenLimit:            model.ContextLength,
			OutputTokenLimit:           model.MaxOutputTokens,
			ContextLength:              model.ContextLength,
			MaxCompletionTokens:        model.MaxOutputTokens,
			SupportedGenerationMethods: []string{"chat.completions"},
			SupportedInputModalities:   modalities,
			SupportedOutputModalities:  []string{"text"},
			SupportedParameters: []string{
				"temperature",
				"top_p",
				"max_tokens",
				"stream",
				"reasoning_effort",
			},
		})
	}
	return out
}

// declaredCapabilities is the single source of truth for the capability set.
// Both plugin.register and the management status page render it, so the plugin
// cannot advertise one set while registering another.
//
// Intentionally unsupported capabilities are listed as false rather than merely
// omitted, so the decision is visible and greppable. See
// docs/capability-matrix.md for why each is not implemented.
func declaredCapabilities() map[string]any {
	return map[string]any{
		// Implemented.
		"model_provider":         true,
		"auth_provider":          true,
		"executor":               true,
		"executor_model_scope":   pluginapi.ExecutorModelScopeBoth,
		"executor_input_formats": []string{"chat-completions"},
		// "claude" is declared so that Anthropic clients get the plugin's own
		// Anthropic SSE frames instead of host translation. CPA's OpenAI→Claude
		// stream translator only accepts frames that already carry a `data:`
		// prefix, which is exactly what the Chat Completions path must not send,
		// and the ExecutorRequest does not reveal the original client protocol.
		// Declaring the format makes the host select and pass it through, which
		// gives the plugin an explicit signal (Format == "claude"). See
		// claude_output.go for the full rationale.
		"executor_output_formats": []string{"chat-completions", "claude"},
		"management_api":          true,
		"quota_provider":          true,
		"usage_plugin":            true,
		"thinking_applier":        true,
		"scheduler":               true,

		// Not implemented.
		"model_registrar":             false,
		"frontend_auth_provider":      false,
		"model_router":                false,
		"request_translator":          false,
		"request_normalizer":          false,
		"request_interceptor":         false,
		"request_lifecycle_plugin":    false,
		"response_translator":         false,
		"response_before_translator":  false,
		"response_after_translator":   false,
		"response_interceptor":        false,
		"stream_chunk_interceptor":    false,
		"websocket_response_observer": false,
		"command_line_plugin":         false,
	}
}
