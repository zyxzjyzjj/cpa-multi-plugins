package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// CPA resolves aliases into Model independently of Payload. Same-format
// requests are not translated by the host, so the body can still contain the
// client's alias (or omit model entirely). Both execution paths must use the
// resolved model before PrepareBody derives SOLO's config_name.
func normalizeExecutorPayload(req *pluginapi.ExecutorRequest) error {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Payload, &body); err != nil || body == nil {
		return &statusError{status: 400, err: fmt.Errorf("invalid chat request: expected a JSON object")}
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		body["model"], _ = json.Marshal(model)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req.Payload = payload
	return nil
}
