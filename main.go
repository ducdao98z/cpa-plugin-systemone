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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginIdentifier = "systemone"
	displayName      = "Open Systemone"
	repositoryURL    = "https://github.com/ducdao98z/cpa-plugin-systemone"
)

// pluginVersion is set from the release tag with -ldflags "-X main.pluginVersion=...".
var pluginVersion = "0.6.1"

var currentConfig atomic.Value

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ManagementAPI         bool     `json:"management_api"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcHostHTTPRequest struct {
	HostCallbackID string              `json:"host_callback_id,omitempty"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
}

func init() {
	hostDo = func(callbackID, method, rawURL string, headers http.Header, body []byte) (httpResult, error) {
		result, errCall := callHost(pluginabi.MethodHostHTTPDo, rpcHostHTTPRequest{
			HostCallbackID: callbackID, Method: method, URL: rawURL, Headers: headers, Body: body,
		})
		if errCall != nil {
			return httpResult{}, errCall
		}
		var resp pluginapi.HTTPResponse
		if errUnmarshal := json.Unmarshal(result, &resp); errUnmarshal != nil {
			return httpResult{}, errUnmarshal
		}
		return httpResult{Status: resp.StatusCode, Body: resp.Body}, nil
	}
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
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
		writeResponse(response, errorEnvelope(pluginabi.NewError("invalid_method", "method is required")))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope(pluginabi.NewError("plugin_error", errHandle.Error())))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { cancelAllDownloads(); cancelRunJobs() }

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: pluginIdentifier, Models: staticModels(loadedConfig())})
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return errorEnvelope(pluginabi.NewError("invalid_request", "systemone does not support streaming", http.StatusBadRequest)), nil
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		var req rpcManagementRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return okEnvelope(handleManagement(loadedConfig(), req.ManagementRequest, req.HostCallbackID))
	default:
		return errorEnvelope(pluginabi.NewError("unknown_method", "unknown method: "+method)), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg, warnings := normalizeConfig(cfg)
	for _, w := range warnings {
		hostLog("warn", "systemone: "+w)
	}
	currentConfig.Store(cfg)
	return nil
}

func loadedConfig() pluginConfig {
	cfg, _ := currentConfig.Load().(pluginConfig)
	return cfg
}

func pluginRegistration() registration {
	presetNames := make([]string, 0, len(presets))
	for name := range presets {
		presetNames = append(presetNames, name)
	}
	sort.Strings(presetNames)
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             displayName,
			Version:          pluginVersion,
			Author:           "ducdao98z",
			GitHubRepository: repositoryURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "nodes", Type: pluginapi.ConfigFieldTypeArray, Description: "The System One decision servers this plugin routes to. Manage them on the plugin's Open Systemone page, which adds a node by URL, checks it, fetches its models and sends a test decision. " +
					"Each item has name, prefix, url, api-keys, headers, proxy-url, models [{id, name}], disabled and preset (one of " + strings.Join(presetNames, ", ") + ")."},
				{Name: "huggingface-token", Type: pluginapi.ConfigFieldTypeString, Description: "A Hugging Face access token. It is only needed to download gated or private models from the Hugging Face tab of the Open Systemone page."},
				{Name: "models-dir", Type: pluginapi.ConfigFieldTypeString, Description: "The folder where models downloaded from Hugging Face are saved, plugins/data/models when empty. A relative path starts at the CLIProxyAPI working directory. The Open Systemone page can browse for it."},
			},
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
			ManagementAPI:         true,
		},
	}
}

func routeModel(raw []byte) ([]byte, error) {
	var req pluginapi.ModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if req.SourceFormat != "openai" {
		return okEnvelope(pluginapi.ModelRouteResponse{})
	}
	model := requestedModel(req.Body)
	if model == "" {
		model = req.RequestedModel
	}
	if _, _, ok := resolveNode(cfg, model); !ok {
		return okEnvelope(pluginapi.ModelRouteResponse{})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "systemone_node",
	})
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	original := req.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	payload, errRun := runDecision(loadedConfig(), original, req.Model, req.HostCallbackID)
	if errRun != nil {
		return errorEnvelope(errRun), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

func hostLog(level, message string) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]string{"level": level, "message": message})
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	cPayload := C.CBytes(rawPayload)
	defer C.free(cPayload)
	var response C.cliproxy_buffer
	callCode := C.call_host_api(cMethod, (*C.uint8_t)(cPayload), C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("host callback %s failed, code=%d", method, int(callCode))
	}
	if !env.OK {
		if env.Error != nil {
			return nil, env.Error
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return env.Result, nil
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(e *pluginabi.Error) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: e})
	return raw
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
