package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// node is one System One decision endpoint, mirroring a 9router custom-systemone node
// or one of its built-in System One providers (see presets).
type node struct {
	Name     string            `yaml:"name" json:"name,omitempty"`
	Preset   string            `yaml:"preset" json:"preset,omitempty"`
	Prefix   string            `yaml:"prefix" json:"prefix,omitempty"`
	URL      string            `yaml:"url" json:"url,omitempty"`
	APIKey   string            `yaml:"api-key" json:"api-key,omitempty"`
	APIKeys  []string          `yaml:"api-keys" json:"api-keys,omitempty"`
	Headers  map[string]string `yaml:"headers" json:"headers,omitempty"`
	ProxyURL string            `yaml:"proxy-url" json:"proxy-url,omitempty"`
	Disabled bool              `yaml:"disabled" json:"disabled,omitempty"`
	Models   []model           `yaml:"models" json:"models,omitempty"`
}

// model is an upstream model id with an optional display name. Alias is an extra
// client-facing name routed without the node prefix and echoed back in responses.
type model struct {
	ID    string `yaml:"id" json:"id"`
	Name  string `yaml:"name" json:"name,omitempty"`
	Alias string `yaml:"alias" json:"alias,omitempty"`
}

// UnmarshalYAML accepts either a plain id string or an {id, name} object.
func (m *model) UnmarshalYAML(unmarshal func(any) error) error {
	var id string
	if unmarshal(&id) == nil {
		m.ID = id
		return nil
	}
	type plain model
	return unmarshal((*plain)(m))
}

// pluginConfig is plugins.configs.systemone. Its enabled key belongs to the plugin base settings:
// the host stops calling a disabled plugin, so it is not read here.
type pluginConfig struct {
	Nodes     []node `yaml:"nodes"`
	HFToken   string `yaml:"huggingface-token"`
	ModelsDir string `yaml:"models-dir"`
}

// sessionPlaceholder in a header value is replaced with a fresh random id per request.
const sessionPlaceholder = "{session}"

// presets mirror the 9router built-in System One providers. Node fields override them.
var presets = map[string]node{
	"opencode": {
		Name:    "OpenCode Free",
		URL:     "https://opencode.ai/zen/v1/systemone",
		APIKeys: []string{"public"},
		Headers: map[string]string{"x-opencode-client": "desktop", "User-Agent": "opencode/1.18.31", "x-opencode-session": sessionPlaceholder},
		Models:  []model{{ID: "jev-1.13-free", Name: "Jev 1.13 Free"}},
	},
	"opencode-zen": {
		Name:    "OpenCode Zen",
		URL:     "https://opencode.ai/zen/v1/systemone",
		Headers: map[string]string{"x-opencode-client": "desktop", "User-Agent": "opencode/1.18.31", "x-opencode-session": sessionPlaceholder},
		Models:  []model{{ID: "jev-1.13", Name: "Jev 1.13"}, {ID: "jev-1.13-free", Name: "Jev 1.13 Free"}},
	},
	"openrouter": {
		Name:    "OpenRouter",
		URL:     "https://openrouter.ai/api/v1/systemone",
		Headers: map[string]string{"HTTP-Referer": repositoryURL, "X-Title": "CLIProxyAPI System One"},
		Models:  []model{{ID: "typesafe/jev-1.13", Name: "Jev 1.13"}},
	},
	"v1m": {
		Name:   "v1m (System One)",
		URL:    "https://v1m.ir/v1/systemone",
		Models: []model{{ID: "rev-latest", Name: "v1m Rev Latest (Calibrated)"}, {ID: "v1m-decision-engine", Name: "v1m Decision Engine"}},
	},
}

// normalizeConfig applies presets, trims fields and drops unusable nodes.
func normalizeConfig(cfg pluginConfig) (pluginConfig, []string) {
	var warnings []string
	nodes := make([]node, 0, len(cfg.Nodes))
	seen := map[string]bool{}
	for i, n := range cfg.Nodes {
		n.Preset = strings.ToLower(strings.TrimSpace(n.Preset))
		if n.Preset != "" {
			p, ok := presets[n.Preset]
			if !ok {
				warnings = append(warnings, fmt.Sprintf("nodes[%d]: unknown preset %q", i, n.Preset))
				continue
			}
			n = applyPreset(n, p)
		}
		n.Prefix = strings.TrimSpace(n.Prefix)
		n.URL = strings.TrimRight(strings.TrimSpace(n.URL), "/")
		n.ProxyURL = strings.TrimSpace(n.ProxyURL)
		n.APIKeys = apiKeys(n)
		n.APIKey = ""
		if n.Name = strings.TrimSpace(n.Name); n.Name == "" {
			n.Name = n.Prefix
		}
		if n.Prefix == "" || strings.Contains(n.Prefix, "/") {
			warnings = append(warnings, fmt.Sprintf("nodes[%d]: prefix is required and must not contain '/'", i))
			continue
		}
		if errURL := validateURL(n.URL); errURL != nil {
			warnings = append(warnings, fmt.Sprintf("node %s: %v", n.Prefix, errURL))
			continue
		}
		if n.ProxyURL != "" && !strings.EqualFold(n.ProxyURL, "direct") {
			if _, errProxy := url.Parse(n.ProxyURL); errProxy != nil {
				warnings = append(warnings, fmt.Sprintf("node %s: invalid proxy-url", n.Prefix))
				continue
			}
		}
		key := strings.ToLower(n.Prefix)
		if seen[key] {
			warnings = append(warnings, fmt.Sprintf("node %s: duplicate prefix ignored", n.Prefix))
			continue
		}
		seen[key] = true
		models := n.Models[:0:0]
		for _, m := range n.Models {
			if m.ID = strings.TrimSpace(m.ID); m.ID != "" {
				m.Name = strings.TrimSpace(m.Name)
				m.Alias = strings.TrimSpace(m.Alias)
				models = append(models, m)
			}
		}
		n.Models = models
		nodes = append(nodes, n)
	}
	cfg.Nodes = nodes
	cfg.HFToken = strings.TrimSpace(cfg.HFToken)
	if cfg.ModelsDir = strings.TrimSpace(cfg.ModelsDir); cfg.ModelsDir == "" {
		cfg.ModelsDir = defaultModelsDir
	}
	return cfg, warnings
}

func applyPreset(n, p node) node {
	if n.Prefix == "" {
		n.Prefix = n.Preset
	}
	if n.Name == "" {
		n.Name = p.Name
	}
	if n.URL == "" {
		n.URL = p.URL
	}
	if n.APIKey == "" && len(n.APIKeys) == 0 {
		n.APIKeys = p.APIKeys
	}
	if len(n.Models) == 0 {
		n.Models = p.Models
	}
	headers := map[string]string{}
	for k, v := range p.Headers {
		headers[k] = v
	}
	for k, v := range n.Headers {
		headers[k] = v
	}
	n.Headers = headers
	return n
}

func validateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	u, errParse := url.Parse(raw)
	if errParse != nil || u.Host == "" {
		return fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url must start with http:// or https://")
	}
	return nil
}

func apiKeys(n node) []string {
	keys := make([]string, 0, len(n.APIKeys)+1)
	for _, k := range append([]string{n.APIKey}, n.APIKeys...) {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// resolveNode maps a model alias or "prefix/model" to its node and the upstream model name.
// A node with an empty models list accepts any model under its prefix.
func resolveNode(cfg pluginConfig, requested string) (node, string, bool) {
	requested = strings.TrimSpace(requested)
	for _, n := range cfg.Nodes {
		if n.Disabled {
			continue
		}
		for _, m := range n.Models {
			if m.Alias != "" && strings.EqualFold(m.Alias, requested) {
				return n, m.ID, true
			}
		}
	}
	prefix, name, ok := strings.Cut(requested, "/")
	if !ok || prefix == "" || name == "" {
		return node{}, "", false
	}
	for _, n := range cfg.Nodes {
		if n.Disabled || !strings.EqualFold(n.Prefix, prefix) {
			continue
		}
		if len(n.Models) == 0 {
			return n, name, true
		}
		for _, m := range n.Models {
			if strings.EqualFold(m.ID, name) {
				return n, m.ID, true
			}
		}
	}
	return node{}, "", false
}

func findNode(cfg pluginConfig, prefix string) (node, bool) {
	for _, n := range cfg.Nodes {
		if strings.EqualFold(n.Prefix, strings.TrimSpace(prefix)) {
			return n, true
		}
	}
	return node{}, false
}

// requestedModel reads the client model from the raw request body.
func requestedModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// buildBody validates the decision payload and swaps in the upstream model name.
// Shape: { model, state, questions: { name: { type, instructions, criteria? } } }.
func buildBody(raw []byte, upstreamModel string) ([]byte, error) {
	var body map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &body); errUnmarshal != nil {
		return nil, fmt.Errorf("invalid JSON body")
	}
	if state, ok := body["state"]; !ok || string(state) == "null" {
		return nil, fmt.Errorf("missing required field: state")
	}
	if q := strings.TrimSpace(string(body["questions"])); !strings.HasPrefix(q, "{") {
		return nil, fmt.Errorf("missing required field: questions")
	}
	delete(body, "stream")
	modelJSON, _ := json.Marshal(upstreamModel)
	body["model"] = modelJSON
	return json.Marshal(body)
}

func requestHeaders(n node, apiKey string) http.Header {
	headers := http.Header{"Content-Type": []string{"application/json"}}
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range n.Headers {
		if v == sessionPlaceholder {
			v = randomID()
		}
		headers.Set(k, v)
	}
	return headers
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var keyCursor atomic.Uint64

// keyOrder rotates the node's keys round-robin; a keyless node yields one empty key.
func keyOrder(keys []string) []string {
	if len(keys) == 0 {
		return []string{""}
	}
	start := int(keyCursor.Add(1)-1) % len(keys)
	return append(append([]string{}, keys[start:]...), keys[:start]...)
}

// shouldTryNextKey reports upstream failures that another key may not hit.
func shouldTryNextKey(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusPaymentRequired ||
		status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500
}

// upstreamErrorMessage extracts a readable message; some servers answer `{ "detail": "..." }`.
func upstreamErrorMessage(status int, body []byte) string {
	var parsed struct {
		Detail  any `json:"detail"`
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil {
		for _, v := range []any{parsed.Error, parsed.Message, parsed.Detail} {
			if m, ok := v.(map[string]any); ok {
				v = m["message"]
			}
			if s, ok := v.(string); ok && s != "" {
				msg = s
				break
			}
		}
	}
	if len(msg) > 512 {
		msg = strings.ToValidUTF8(msg[:512], "") + "..."
	}
	return fmt.Sprintf("systemone upstream %d: %s", status, msg)
}

// modelsURL derives the model list endpoint beside the decision endpoint (.../v1/systemone → .../v1/models).
func modelsURL(systemoneURL string) string {
	value := strings.TrimRight(strings.TrimSpace(systemoneURL), "/")
	if strings.HasSuffix(value, "/models") {
		return value
	}
	return strings.TrimSuffix(value, "/systemone") + "/models"
}

// parseModelList reads OpenAI ({data}), Ollama ({models}) or plain array model lists.
func parseModelList(raw []byte, prefix string) []model {
	var list []any
	var wrapped struct {
		Data   []any `json:"data"`
		Models []any `json:"models"`
	}
	if json.Unmarshal(raw, &list) != nil {
		if json.Unmarshal(raw, &wrapped) != nil {
			return nil
		}
		list = wrapped.Data
		if len(list) == 0 {
			list = wrapped.Models
		}
	}
	seen := map[string]bool{}
	models := []model{}
	for _, item := range list {
		var id, name string
		switch v := item.(type) {
		case string:
			id = v
		case map[string]any:
			for _, k := range []string{"id", "name", "model"} {
				if s, ok := v[k].(string); ok && s != "" {
					id = s
					break
				}
			}
			name, _ = v["name"].(string)
		}
		id = strings.TrimPrefix(strings.TrimSpace(id), prefix+"/")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if name == id {
			name = ""
		}
		models = append(models, model{ID: id, Name: name})
	}
	return models
}

func healthURL(systemoneURL string) string {
	u, errParse := url.Parse(systemoneURL)
	if errParse != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/health"
}

func probeBody(modelID string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":     modelID,
		"state":     "ping",
		"questions": map[string]any{"probe": map[string]any{"type": "noul", "instructions": "Is this a probe?"}},
	})
	return raw
}
