# System One plugin for CLIProxyAPI

Adds [System One](https://opencode.ai) decision nodes to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
A decision request (`{model, state, questions}`) goes to the node chosen by the model prefix and
the node's JSON answer comes back unchanged. Any System One-compatible server can be a node: a
local model server (e.g. Winnow 12B) or a hosted lane (OpenCode, OpenRouter, v1m).

- Add nodes by URL, with several API keys (round-robin with fallback), extra headers and a per-node proxy.
- Configured models show up in `GET /v1/models` as `prefix/model`.
- A **System One** page in the management panel: node list, Check (with latency), Fetch models
  from the node, and an example builder that sends test decisions.

Requires CLIProxyAPI v8 with plugins enabled. Streaming is not supported (decisions are a single JSON response).

## Install

**Plugin store:** open *Plugins → Store* in the management panel and install **System One**.

**Manually:** download `systemone_<version>_<os>_<arch>.zip` from
[Releases](https://github.com/ducdao98z/cpa-plugin-systemone/releases), extract
`systemone.so` / `.dylib` / `.dll` into the plugins directory (`plugins/` by default) and restart.

**From source** (Go 1.26+ and a C compiler):

```bash
go build -buildmode=c-shared -buildvcs=false -o systemone.so .
```

For the official Docker image (Debian bookworm), build in a matching container so glibc fits:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm \
  sh -c 'go build -buildmode=c-shared -buildvcs=false -o systemone.so . && rm -f systemone.h'
```

## Configure

Enable the plugin under its base settings, then add nodes on the **System One** page
(PLUGINS in the sidebar) or in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    systemone:
      enabled: true
      priority: 10
      nodes:
        - name: "Winnow Local"                        # display name (defaults to prefix)
          prefix: "winnow"                            # model prefix: winnow/Winnow-12B
          url: "http://127.0.0.1:3333/v1/systemone"   # full decision endpoint
          api-keys: ["key-a", "key-b"]                # optional; api-key: "..." also works
          headers:                                    # optional extra headers
            X-Custom: "value"                         # "{session}" = random id per request
          proxy-url: ""                               # "" = global proxy, "direct", or http/https/socks5 URL
          disabled: false
          models:                                     # empty list accepts any model under the prefix
            - "Winnow-12B"                            # plain id, or
            - { id: "Winnow-12B-bf16", name: "Winnow 12B BF16" }
        - preset: "opencode"                          # prefix defaults to the preset name
        - preset: "openrouter"
          prefix: "or"                                # → or/typesafe/jev-1.13
          api-keys: ["sk-or-..."]
```

| Preset | URL | Models | Key |
|---|---|---|---|
| `opencode` | `https://opencode.ai/zen/v1/systemone` | `jev-1.13-free` | `public` (built in) |
| `opencode-zen` | `https://opencode.ai/zen/v1/systemone` | `jev-1.13`, `jev-1.13-free` | required |
| `openrouter` | `https://openrouter.ai/api/v1/systemone` | `typesafe/jev-1.13` | required |
| `v1m` | `https://v1m.ir/v1/systemone` | `rev-latest`, `v1m-decision-engine` | required |

With several keys, requests start round-robin and move to the next key on 401/402/403/429/5xx or
network errors. Other errors (e.g. 422) return at once with the upstream status and message.

## Use

Send the decision payload to `POST /v1/chat/completions` with a normal CLIProxyAPI API key. The
plugin claims any request whose model matches a node. Hosts that expose `POST /v1/systemone`
accept the same request there.

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer <api-key>' -H 'Content-Type: application/json' \
  -d '{"model":"winnow/Winnow-12B","state":{"ticket":"I was charged twice."},
       "questions":{
         "refund":{"type":"noul","instructions":"Does the customer request a refund?"},
         "department":{"type":"choice","instructions":"Which department?","criteria":{"billing":"Payments","technical":"Defects"}},
         "urgency":{"type":"score","instructions":"How urgent?","criteria":["Routine","Time-sensitive","Emergency"]}}}'
```

Question types: `noul` (yes/no probability), `choice` (`criteria` is a label→description object),
`score` (`criteria` is an ordered list of levels).

## System One page

Opened from the management panel it reuses the panel's login; opened on its own
(`/v0/resource/plugins/systemone/ui`) it asks for the management key and keeps it in this
browser's local storage until you click **Forget key**.

- Node list with status, Check, Open, Delete.
- Add/edit node: preset or custom, name, prefix, URL, API keys, headers, proxy URL.
- Models: add, rename, remove, or **Fetch from API** (`.../v1/models` beside the URL).
- Example: build `noul` / `choice` / `score` questions, Send, and see the request, a `curl`, the
  response and the latency.

Check and Send on a saved, enabled node go through `POST /v1/chat/completions` with the first client
API key, like a real client, so they show up in usage statistics. Unsaved changes and disabled nodes
are tested directly through the management routes below (Check: `/health`, then a probe decision
when a key is set).

Saving writes `plugins.configs.systemone.nodes` through `/v8/management/config`; the plugin reloads
automatically.

### Management API

All routes need the management key.

| Route | Purpose |
|---|---|
| `GET /v0/management/systemone/presets` | Built-in lanes (url, headers, models) |
| `GET /v0/management/systemone/nodes` | Effective nodes after presets and validation |
| `POST /v0/management/systemone/check` | `{node, model}`: `/health`, then a probe decision when keyed |
| `POST /v0/management/systemone/models` | `{node}`: upstream model list |
| `POST /v0/management/systemone/test` | `{node, model, payload}`: send a decision through the node |

`node` is either `{"prefix": "<saved prefix>"}` or a full unsaved node (with `url` or `preset`).

## Release

Push a `v<version>` tag. The Build workflow tests, builds linux/darwin/windows libraries, and
publishes `systemone_<version>_<os>_<arch>.zip` plus `checksums.txt` in the layout the
CLIProxyAPI plugin store installs.

## License

MIT
