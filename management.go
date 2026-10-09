package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ui is the System One console, served as a plugin resource. It holds no data itself:
// every call it makes is authenticated with the management key the viewer enters.
//
//go:embed ui.html
var ui []byte

const (
	managementBase = "/v0/management/systemone"
	uiResourcePath = "/ui"
)

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type resourceRoute struct {
	Path        string
	Menu        string
	Description string
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []managementRoute{
			{Method: http.MethodGet, Path: managementBase + "/presets", Description: "Built-in System One lanes (url, headers, models)."},
			{Method: http.MethodGet, Path: managementBase + "/nodes", Description: "Effective System One nodes after presets and validation."},
			{Method: http.MethodPost, Path: managementBase + "/check", Description: "Check a node: GET /health, then a probe decision when a key is set."},
			{Method: http.MethodPost, Path: managementBase + "/models", Description: "Fetch a node's upstream model list (.../v1/models)."},
			{Method: http.MethodPost, Path: managementBase + "/test", Description: "Send a decision payload to a node."},
		},
		"resources": []resourceRoute{
			{Path: uiResourcePath, Menu: "System One", Description: "Add System One nodes, check them, fetch models and send test decisions."},
		},
	}
}

// nodeRequest targets a saved node by prefix, or an unsaved node from the form (url set).
type nodeRequest struct {
	Node    node            `json:"node"`
	Model   string          `json:"model"`
	Payload json.RawMessage `json:"payload"`
}

func (r nodeRequest) resolve(cfg pluginConfig) (node, bool) {
	if strings.TrimSpace(r.Node.URL) == "" && r.Node.Preset == "" {
		return findNode(cfg, r.Node.Prefix)
	}
	n := r.Node
	if n.Prefix == "" {
		n.Prefix = "check"
	}
	normalized, _ := normalizeConfig(pluginConfig{Nodes: []node{n}})
	if len(normalized.Nodes) == 0 {
		return node{}, false
	}
	return normalized.Nodes[0], true
}

func handleManagement(cfg pluginConfig, req pluginapi.ManagementRequest, callbackID string) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	if strings.HasSuffix(path, "/resource/plugins/"+pluginIdentifier+uiResourcePath) {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
			Body:       ui,
		}
	}
	switch path {
	case managementBase + "/presets":
		return jsonResponse(http.StatusOK, map[string]any{"presets": presetList()})
	case managementBase + "/nodes":
		return jsonResponse(http.StatusOK, map[string]any{"nodes": cfg.Nodes})
	}

	var body nodeRequest
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
	}
	n, ok := body.resolve(cfg)
	if !ok {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "node not found or invalid (prefix and http(s) url are required)"})
	}
	switch path {
	case managementBase + "/check":
		return jsonResponse(http.StatusOK, checkNode(n, body.Model, callbackID))
	case managementBase + "/models":
		models, errFetch := fetchModels(n, callbackID)
		if errFetch != nil {
			return jsonResponse(http.StatusOK, map[string]any{"error": errFetch.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"models": models})
	case managementBase + "/test":
		return jsonResponse(http.StatusOK, testDecision(n, body.Model, body.Payload, callbackID))
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown route"})
	}
}

func presetList() []map[string]any {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		p := presets[name]
		out = append(out, map[string]any{
			"preset": name, "name": p.Name, "url": p.URL, "headers": p.Headers,
			"models": p.Models, "key_required": len(p.APIKeys) == 0,
		})
	}
	return out
}

// checkNode mirrors 9router's Check button: a keyless node is confirmed with GET /health
// so the check does not wait on inference; a keyed node is probed because /health skips auth.
func checkNode(n node, modelID, callbackID string) map[string]any {
	var health map[string]any
	if hu := healthURL(n.URL); hu != "" {
		if resp, errSend := send(n, callbackID, http.MethodGet, hu, http.Header{}, nil); errSend == nil && resp.Status == http.StatusOK {
			health = map[string]any{}
			_ = json.Unmarshal(resp.Body, &health)
			if len(n.APIKeys) == 0 {
				return map[string]any{"valid": true, "method": "health", "health": health}
			}
		}
	}
	if modelID == "" && len(n.Models) > 0 {
		modelID = n.Models[0].ID
	}
	key := ""
	if len(n.APIKeys) > 0 {
		key = n.APIKeys[0]
	}
	resp, errSend := send(n, callbackID, http.MethodPost, n.URL, requestHeaders(n, key), probeBody(modelID))
	switch {
	case errSend == nil && resp.Status >= 200 && resp.Status < 300:
		return map[string]any{"valid": true, "method": "systemone", "model": modelID}
	case errSend == nil && (resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden):
		return map[string]any{"valid": false, "error": "API key unauthorized"}
	case health != nil:
		return map[string]any{"valid": true, "method": "health", "health": health}
	case errSend != nil:
		return map[string]any{"valid": false, "error": errSend.Error()}
	default:
		return map[string]any{"valid": false, "error": upstreamErrorMessage(resp.Status, resp.Body)}
	}
}

func fetchModels(n node, callbackID string) ([]model, error) {
	headers := http.Header{"Accept": []string{"application/json"}}
	if len(n.APIKeys) > 0 {
		headers.Set("Authorization", "Bearer "+n.APIKeys[0])
	}
	resp, errSend := send(n, callbackID, http.MethodGet, modelsURL(n.URL), headers, nil)
	if errSend != nil {
		return nil, errSend
	}
	if resp.Status != http.StatusOK {
		return nil, errorString(upstreamErrorMessage(resp.Status, resp.Body))
	}
	if !json.Valid(resp.Body) {
		return nil, errorString("model API did not return JSON")
	}
	return parseModelList(resp.Body, n.Prefix), nil
}

// testDecision sends the example payload the way /v1/systemone would, reporting the raw result.
func testDecision(n node, modelID string, payload json.RawMessage, callbackID string) map[string]any {
	if modelID == "" && len(n.Models) > 0 {
		modelID = n.Models[0].ID
	}
	routed := n
	routed.Models = nil // accept the chosen model even when it is not saved yet
	cfg := pluginConfig{Nodes: []node{routed}}
	var body map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return map[string]any{"ok": false, "status": http.StatusBadRequest, "error": "payload must be a JSON object"}
	}
	modelJSON, _ := json.Marshal(n.Prefix + "/" + modelID)
	body["model"] = modelJSON
	raw, _ := json.Marshal(body)

	started := time.Now()
	out, errRun := runDecision(cfg, raw, "", callbackID)
	elapsed := time.Since(started).Milliseconds()
	if errRun != nil {
		return map[string]any{"ok": false, "status": errRun.HTTPStatus, "error": errRun.Message, "latency_ms": elapsed}
	}
	return map[string]any{"ok": true, "status": http.StatusOK, "response": json.RawMessage(out), "latency_ms": elapsed}
}

type errorString string

func (e errorString) Error() string { return string(e) }
