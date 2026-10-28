package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type httpResult struct {
	Status int
	Body   []byte
}

// hostDo sends a request through the CPA host HTTP client (global proxy, request-log).
// main.go wires it to the host.http.do callback; tests replace it.
var hostDo func(callbackID, method, rawURL string, headers http.Header, body []byte) (httpResult, error)

var proxyClients sync.Map // proxy-url -> *http.Client

// send uses the host client unless the node pins its own proxy-url ("direct" bypasses every proxy).
func send(n node, callbackID, method, rawURL string, headers http.Header, body []byte) (httpResult, error) {
	if n.ProxyURL == "" {
		return hostDo(callbackID, method, rawURL, headers, body)
	}
	req, errNew := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if errNew != nil {
		return httpResult{}, errNew
	}
	req.Header = headers
	resp, errDo := proxyClient(n.ProxyURL).Do(req)
	if errDo != nil {
		return httpResult{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return httpResult{}, errRead
	}
	return httpResult{Status: resp.StatusCode, Body: respBody}, nil
}

func proxyClient(proxyURL string) *http.Client {
	if c, ok := proxyClients.Load(proxyURL); ok {
		return c.(*http.Client)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if !strings.EqualFold(proxyURL, "direct") {
		if u, errParse := url.Parse(proxyURL); errParse == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	c, _ := proxyClients.LoadOrStore(proxyURL, &http.Client{Transport: transport})
	return c.(*http.Client)
}

// runDecision forwards one decision request, rotating through the node's keys on
// auth, quota, rate-limit and server failures like 9router's account fallback.
func runDecision(cfg pluginConfig, original []byte, fallbackModel, callbackID string) ([]byte, *pluginabi.Error) {
	requested := requestedModel(original)
	if requested == "" {
		requested = fallbackModel
	}
	n, upstreamModel, ok := resolveNode(cfg, requested)
	if !ok {
		return nil, pluginabi.NewError("model_not_found", "no systemone node for model "+requested, http.StatusNotFound)
	}
	body, errBody := buildBody(original, upstreamModel)
	if errBody != nil {
		return nil, pluginabi.NewError("invalid_request", errBody.Error(), http.StatusBadRequest)
	}

	var lastErr *pluginabi.Error
	for _, key := range keyOrder(n.APIKeys) {
		resp, errSend := send(n, callbackID, http.MethodPost, n.URL, requestHeaders(n, key), body)
		if errSend != nil {
			lastErr = pluginabi.NewError("upstream_unreachable", fmt.Sprintf("systemone %s: %v", n.Prefix, errSend), http.StatusBadGateway)
			continue
		}
		if resp.Status >= 200 && resp.Status < 300 {
			if !json.Valid(resp.Body) {
				return nil, pluginabi.NewError("upstream_error", "systemone upstream returned invalid JSON", http.StatusBadGateway)
			}
			for _, m := range n.Models {
				if m.Alias != "" && strings.EqualFold(m.Alias, strings.TrimSpace(requested)) {
					return withModel(resp.Body, m.Alias), nil
				}
			}
			return resp.Body, nil
		}
		lastErr = pluginabi.NewError("upstream_error", upstreamErrorMessage(resp.Status, resp.Body), resp.Status)
		if !shouldTryNextKey(resp.Status) {
			break
		}
	}
	return nil, lastErr
}

// withModel sets the response "model" field so an aliased model never shows its upstream id.
func withModel(body []byte, model string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	obj["model"], _ = json.Marshal(model)
	out, errMarshal := json.Marshal(obj)
	if errMarshal != nil {
		return body
	}
	return out
}

// staticModels lists every alias and every configured "prefix/model" so clients see them in /v1/models.
func staticModels(cfg pluginConfig) []pluginapi.ModelInfo {
	var out []pluginapi.ModelInfo
	for _, n := range cfg.Nodes {
		if n.Disabled {
			continue
		}
		for _, m := range n.Models {
			name := m.Name
			if name == "" {
				name = m.ID
			}
			if m.Alias != "" {
				out = append(out, pluginapi.ModelInfo{
					ID: m.Alias, Object: "model", OwnedBy: n.Name, Type: "systemone",
					DisplayName: name, Name: m.ID, UserDefined: true,
				})
			}
			out = append(out, pluginapi.ModelInfo{
				ID: n.Prefix + "/" + m.ID, Object: "model", OwnedBy: n.Name, Type: "systemone",
				DisplayName: name, Name: m.ID, UserDefined: true,
			})
		}
	}
	return out
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(v)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}
