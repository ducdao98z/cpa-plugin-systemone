package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadTestConfig(t *testing.T, raw string) pluginConfig {
	t.Helper()
	cfg := pluginConfig{}
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _ = normalizeConfig(cfg)
	return cfg
}

func TestConfigPresetsAndModels(t *testing.T) {
	cfg := loadTestConfig(t, `
nodes:
  - prefix: winnow
    url: http://127.0.0.1:3333/v1/systemone/
    api-key: k1
    api-keys: [k2]
    models: ["Winnow-12B", {id: other, name: Other}]
  - preset: opencode
  - preset: openrouter
    prefix: or
    api-keys: [sk-or]
  - preset: nope
  - prefix: bad
    url: ftp://x
  - prefix: winnow
    url: http://dup/v1/systemone
`)
	if len(cfg.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3: %+v", len(cfg.Nodes), cfg.Nodes)
	}
	w := cfg.Nodes[0]
	if w.URL != "http://127.0.0.1:3333/v1/systemone" || len(w.APIKeys) != 2 || w.Models[1].Name != "Other" {
		t.Fatalf("winnow node = %+v", w)
	}
	oc := cfg.Nodes[1]
	if oc.Prefix != "opencode" || oc.APIKeys[0] != "public" || oc.Headers["x-opencode-session"] != sessionPlaceholder {
		t.Fatalf("opencode preset = %+v", oc)
	}
	if h := requestHeaders(oc, "public"); h.Get("x-opencode-session") == sessionPlaceholder || h.Get("Authorization") != "Bearer public" {
		t.Fatalf("headers = %v", h)
	}
	if _, m, ok := resolveNode(cfg, "or/typesafe/jev-1.13"); !ok || m != "typesafe/jev-1.13" {
		t.Fatalf("openrouter model resolve = %q %v", m, ok)
	}
}

func TestResolveNode(t *testing.T) {
	cfg := loadTestConfig(t, `
nodes:
  - {prefix: winnow, url: "http://w/v1/systemone", models: [Winnow-12B]}
  - {prefix: any, url: "http://x/v1/systemone"}
  - {prefix: off, url: "http://x/v1/systemone", disabled: true}
`)
	cases := []struct {
		in    string
		ok    bool
		model string
	}{
		{"winnow/winnow-12b", true, "Winnow-12B"},
		{"winnow/other", false, ""},
		{"any/whatever", true, "whatever"},
		{"off/m", false, ""},
		{"gpt-5", false, ""},
		{"winnow/", false, ""},
	}
	for _, c := range cases {
		_, got, ok := resolveNode(cfg, c.in)
		if ok != c.ok || got != c.model {
			t.Errorf("resolveNode(%q) = %q,%v; want %q,%v", c.in, got, ok, c.model, c.ok)
		}
	}
}

func TestBuildBody(t *testing.T) {
	raw := []byte(`{"model":"winnow/winnow-12b","stream":false,"state":"hi","questions":{"is_urgent":{"type":"noul","instructions":"Urgent?"}}}`)
	out, err := buildBody(raw, "Winnow-12B")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["model"] != "Winnow-12B" || got["state"] != "hi" || got["questions"] == nil {
		t.Fatalf("unexpected body: %s", out)
	}
	if _, ok := got["stream"]; ok {
		t.Fatalf("stream should be dropped: %s", out)
	}
	for _, bad := range []string{`{"questions":{}}`, `{"state":null,"questions":{}}`, `{"state":"x"}`, `{"state":"x","questions":[]}`, `not json`} {
		if _, err := buildBody([]byte(bad), "m"); err == nil {
			t.Errorf("buildBody(%s) should fail", bad)
		}
	}
}

func TestRunDecisionKeyFallback(t *testing.T) {
	cfg := loadTestConfig(t, `
nodes:
  - {prefix: w, url: "http://w/v1/systemone", api-keys: [bad, good]}
`)
	var seen []string
	hostDo = func(_, _, _ string, h http.Header, _ []byte) (httpResult, error) {
		seen = append(seen, h.Get("Authorization"))
		if h.Get("Authorization") == "Bearer good" {
			return httpResult{Status: 200, Body: []byte(`{"answers":{}}`)}, nil
		}
		return httpResult{Status: 401, Body: []byte(`{"detail":"bad key"}`)}, nil
	}
	body := []byte(`{"model":"w/m","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`)
	for i := 0; i < 2; i++ {
		seen = nil
		out, errRun := runDecision(cfg, body, "", "")
		if errRun != nil || string(out) != `{"answers":{}}` {
			t.Fatalf("run %d: out=%s err=%v seen=%v", i, out, errRun, seen)
		}
	}

	hostDo = func(_, _, _ string, _ http.Header, _ []byte) (httpResult, error) {
		return httpResult{Status: 422, Body: []byte(`{"detail":"bad state"}`)}, nil
	}
	seen = nil
	_, errRun := runDecision(cfg, body, "", "")
	if errRun == nil || errRun.HTTPStatus != 422 || errRun.Message != "systemone upstream 422: bad state" {
		t.Fatalf("422 should not fall back and keep status: %+v", errRun)
	}
}

func TestModelList(t *testing.T) {
	if got := modelsURL("http://h:3333/v1/systemone"); got != "http://h:3333/v1/models" {
		t.Fatalf("modelsURL = %s", got)
	}
	raw := []byte(`{"models":[{"name":"Winnow-12B","model":"Winnow-12B"}],"data":[{"id":"w/Winnow-12B"},{"id":"Winnow-12B"},{"id":"x","name":"X"}]}`)
	got := parseModelList(raw, "w")
	if len(got) != 2 || got[0].ID != "Winnow-12B" || got[1].Name != "X" {
		t.Fatalf("parseModelList = %+v", got)
	}
	if got := parseModelList([]byte(`["a","a","b"]`), ""); len(got) != 2 {
		t.Fatalf("plain list = %+v", got)
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"detail":"bad state"}`:       "systemone upstream 422: bad state",
		`{"error":{"message":"nope"}}`: "systemone upstream 422: nope",
		`plain`:                        "systemone upstream 422: plain",
	} {
		if got := upstreamErrorMessage(422, []byte(body)); got != want {
			t.Errorf("upstreamErrorMessage(%s) = %q; want %q", body, got, want)
		}
	}
}

func TestManagementTestUsesUnsavedNode(t *testing.T) {
	var gotURL, gotBody string
	hostDo = func(_, _, rawURL string, _ http.Header, body []byte) (httpResult, error) {
		gotURL, gotBody = rawURL, string(body)
		return httpResult{Status: 200, Body: []byte(`{"answers":{}}`)}, nil
	}
	req := nodeRequest{
		Node:    node{Prefix: "new", URL: "http://new/v1/systemone/"},
		Model:   "M1",
		Payload: json.RawMessage(`{"state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`),
	}
	n, ok := req.resolve(pluginConfig{})
	if !ok {
		t.Fatal("unsaved node should resolve")
	}
	out := testDecision(n, req.Model, req.Payload, "")
	if out["ok"] != true || gotURL != "http://new/v1/systemone" || !strings.Contains(gotBody, `"model":"M1"`) {
		t.Fatalf("out=%v url=%s body=%s", out, gotURL, gotBody)
	}
	if _, ok := (nodeRequest{Node: node{Prefix: "missing"}}).resolve(pluginConfig{}); ok {
		t.Fatal("unknown saved prefix should not resolve")
	}
}
