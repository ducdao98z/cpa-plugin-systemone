package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ui is the Open Systemone console, served as a plugin resource. It holds no data itself:
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
			{Method: http.MethodGet, Path: managementBase + "/presets", Description: "The built-in System One lanes, with their URL, headers and models."},
			{Method: http.MethodGet, Path: managementBase + "/nodes", Description: "The nodes the plugin actually uses, with presets filled in. Nodes with errors, such as a missing prefix or a bad URL, are left out and listed as warnings."},
			{Method: http.MethodPost, Path: managementBase + "/check", Description: "Checks a node by calling its GET /health, then sends a small probe decision if the node has a key."},
			{Method: http.MethodPost, Path: managementBase + "/models", Description: "Fetches the model list from a node's server (its /v1/models)."},
			{Method: http.MethodPost, Path: managementBase + "/test", Description: "Sends a decision payload to a node and returns its answer."},
			{Method: http.MethodGet, Path: managementBase + "/hf/status", Description: "Shows whether a Hugging Face token is set and which account it belongs to, the downloads of this session and the models already in models-dir."},
			{Method: http.MethodGet, Path: managementBase + "/hf/machine", Description: "Describes the computer CLIProxyAPI runs on: CPU, memory, free disk space in models-dir and NVIDIA GPUs."},
			{Method: http.MethodGet, Path: managementBase + "/hf/index", Description: "Lists the models of the Jev Decision Index that have weights on Hugging Face, with their size, score and latency. Add ?refresh=1 to reload it."},
			{Method: http.MethodPost, Path: managementBase + "/hf/files", Description: "Lists the files of a Hugging Face model with their sizes."},
			{Method: http.MethodPost, Path: managementBase + "/hf/download", Description: "Starts downloading a Hugging Face model, or some of its files, into models-dir. An interrupted download continues where it stopped."},
			{Method: http.MethodPost, Path: managementBase + "/hf/cancel", Description: "Stops a running download. The part already downloaded is kept."},
			{Method: http.MethodPost, Path: managementBase + "/hf/delete", Description: "Deletes a downloaded model from models-dir."},
			{Method: http.MethodPost, Path: managementBase + "/hf/gguf", Description: "Lists the GGUF conversions of a model on Hugging Face, which llama.cpp can run."},
			{Method: http.MethodGet, Path: managementBase + "/run/status", Description: "Shows whether Docker is reachable, the model servers the plugin started and the progress of the ones being set up."},
			{Method: http.MethodPost, Path: managementBase + "/run/files", Description: "Lists the downloaded GGUF files of a model and whether they already carry System One decision metadata."},
			{Method: http.MethodPost, Path: managementBase + "/run/start", Description: "Installs and starts a downloaded GGUF model in a llama.cpp container: adds decision metadata when missing, pulls the image if needed and waits until the model is loaded."},
			{Method: http.MethodPost, Path: managementBase + "/run/action", Description: "Stops, starts, restarts or removes a model server the plugin started."},
			{Method: http.MethodPost, Path: managementBase + "/run/logs", Description: "Returns the last lines a model server printed."},
			{Method: http.MethodGet, Path: managementBase + "/hf/dirs", Description: "Lists the subfolders of a folder (?path=, models-dir when empty) so the page can pick where models are saved, and says whether it can be written to."},
		},
		"resources": []resourceRoute{
			{Path: uiResourcePath, Menu: displayName, Description: "Add System One decision servers and test them, and download decision models from Hugging Face and run them with llama.cpp."},
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
	if strings.HasPrefix(path, managementBase+"/hf/") {
		return handleHF(cfg, path, req)
	}
	if strings.HasPrefix(path, managementBase+"/run/") {
		return handleRun(cfg, path, req)
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

type hfRequest struct {
	Repo  string   `json:"repo"`
	Files []string `json:"files"`
}

func handleHF(cfg pluginConfig, path string, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch path {
	case managementBase + "/hf/status":
		user, errUser := hfUser(cfg.HFToken)
		out := map[string]any{"token_set": cfg.HFToken != "", "user": user, "models_dir": cfg.ModelsDir, "downloads": downloadList(), "local": localModels(cfg)}
		if errUser != nil {
			out["token_error"] = errUser.Error()
		}
		return jsonResponse(http.StatusOK, out)
	case managementBase + "/hf/machine":
		return jsonResponse(http.StatusOK, machine(cfg))
	case managementBase + "/hf/dirs":
		return jsonResponse(http.StatusOK, listDirs(cfg, req.Query.Get("path")))
	case managementBase + "/hf/index":
		generated, models, errIndex := jevIndex(cfg.HFToken, req.Query.Get("refresh") != "")
		if errIndex != nil {
			return jsonResponse(http.StatusBadGateway, map[string]any{"error": errIndex.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"generated": generated, "source": hfBase + hfIndexSpace, "models": models})
	}

	var body hfRequest
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil || !validRepo(body.Repo) {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "a valid repo (owner/name) is required"})
	}
	switch path {
	case managementBase + "/hf/gguf":
		builds, errBuilds := ggufBuilds(cfg.HFToken, body.Repo)
		if errBuilds != nil {
			return jsonResponse(http.StatusBadGateway, map[string]any{"error": errBuilds.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"repos": builds})
	case managementBase + "/hf/files":
		files, errList := listRepoFiles(cfg.HFToken, body.Repo)
		if errList != nil {
			return jsonResponse(http.StatusBadGateway, map[string]any{"error": errList.Error()})
		}
		dir := repoDir(cfg, body.Repo)
		have := map[string]bool{}
		for _, f := range files {
			have[f.Path] = fileSize(filepath.Join(dir, filepath.FromSlash(f.Path))) == f.Size
		}
		return jsonResponse(http.StatusOK, map[string]any{"files": files, "downloaded": have, "path": dir})
	case managementBase + "/hf/download":
		d, errStart := startDownload(cfg, body.Repo, body.Files)
		if errStart != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errStart.Error()})
		}
		return jsonResponse(http.StatusOK, d)
	case managementBase + "/hf/cancel":
		return jsonResponse(http.StatusOK, map[string]any{"canceled": cancelDownload(body.Repo)})
	case managementBase + "/hf/delete":
		if errDelete := deleteModel(cfg, body.Repo); errDelete != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errDelete.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"deleted": true})
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown route"})
}

func handleRun(cfg pluginConfig, path string, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx := context.Background()
	if path == managementBase+"/run/status" {
		out := map[string]any{"jobs": jobList(), "runners": []runner{}}
		env, errEnv := currentDockerEnv(ctx)
		if errEnv != nil {
			out["docker_error"] = errEnv.Error()
			return jsonResponse(http.StatusOK, out)
		}
		out["docker"] = env
		if runners, errList := listRunners(ctx); errList != nil {
			out["docker_error"] = errList.Error()
		} else {
			out["runners"] = runners
		}
		return jsonResponse(http.StatusOK, out)
	}
	var body struct {
		runRequest
		Action string `json:"action"`
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
	}
	switch path {
	case managementBase + "/run/files":
		if !validRepo(body.Repo) {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "a valid repo (owner/name) is required"})
		}
		return jsonResponse(http.StatusOK, map[string]any{"files": localGGUF(cfg, body.Repo), "path": repoDir(cfg, body.Repo)})
	case managementBase + "/run/start":
		job, errStart := startRunner(cfg, body.runRequest)
		if errStart != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errStart.Error()})
		}
		return jsonResponse(http.StatusOK, job)
	case managementBase + "/run/action":
		if errAction := runnerAction(ctx, body.Name, body.Action); errAction != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errAction.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": true})
	case managementBase + "/run/logs":
		if !runnerName.MatchString(body.Name) {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid model name"})
		}
		logs, errLogs := runnerLogs(ctx, runnerPrefix+body.Name, 200)
		if errLogs != nil {
			return jsonResponse(http.StatusBadGateway, map[string]any{"error": errLogs.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"logs": logs})
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown route"})
}

type errorString string

func (e errorString) Error() string { return string(e) }
