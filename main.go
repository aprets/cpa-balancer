// cpa-balancer: CLIProxyAPI scheduler plugin. See DESIGN.md.
//
// This file is only the C ABI bridge and RPC dispatch. All behaviour lives in
// balancer.go and quota.go so it can be unit tested without cgo.
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

static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }

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
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginName = "cpa-balancer"
const pluginVersion = "0.1.0"

var bal = newBalancer(hostLog)

func init() { bal.authJSON, bal.authList = hostAuthJSON, hostAuthList }

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
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	out, err := handleMethod(C.GoString(method), raw)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { bal.shutdown() }

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

type managementRoutes struct {
	Routes []pluginapi.ManagementRoute `json:"routes"`
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
		}
		var cfg config
		if len(req.ConfigYAML) > 0 {
			if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
				return nil, err
			}
		}
		bal.configure(cfg)
		return okEnvelope(registration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata: pluginapi.Metadata{
				Name:             pluginName,
				Version:          pluginVersion,
				Author:           "localflare",
				GitHubRepository: "https://github.com/aprets/localflare",
				ConfigFields:     configFields,
			},
			Capabilities: map[string]bool{"scheduler": true, "usage_plugin": true, "management_api": true},
		})
	case pluginabi.MethodPluginShutdown, pluginabi.MethodPluginQuiesce:
		bal.shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return okEnvelope(bal.pick(req))
	case pluginabi.MethodUsageHandle:
		var rec pluginapi.UsageRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, err
		}
		bal.usage(rec)
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRoutes{Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/" + pluginName + "/state"},
		}})
	case pluginabi.MethodManagementHandle:
		body, err := json.Marshal(bal.state())
		if err != nil {
			return nil, err
		}
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

var configFields = []pluginapi.ConfigField{
	{Name: "shadow", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Log decisions but leave routing to CPA. Default true."},
	{Name: "k", Type: pluginapi.ConfigFieldTypeNumber, Description: "Exponent on urgency. 1 = proportional, higher leans harder to the soonest reset. Default 1."},
	{Name: "horizon_hours", Type: pluginapi.ConfigFieldTypeNumber, Description: "Expected session lifetime in hours, added to time-until-reset. Default 6."},
	{Name: "state_file", Type: pluginapi.ConfigFieldTypeString, Description: "Where bindings and quota snapshot persist across restarts."},
	{Name: "probe", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Pull usage directly from the upstream usage endpoint for accounts with no recent observation. Default true."},
	{Name: "probe_stale_minutes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Observation age after which an account is probed. Default 60."},
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
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

// hostCall invokes a host callback and returns the result payload.
func hostCall(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	var resp C.cliproxy_buffer
	rc := C.call_host_api(cMethod, req, C.size_t(len(payload)), &resp)
	var raw []byte
	if resp.ptr != nil {
		raw = C.GoBytes(unsafe.Pointer(resp.ptr), C.int(resp.len))
		C.free_host_buffer(resp.ptr, resp.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed (%d): %s", method, int(rc), string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("host call %s: bad envelope: %w", method, err)
	}
	if !env.OK {
		msg := "unknown error"
		if env.Error != nil {
			msg = env.Error.Code + ": " + env.Error.Message
		}
		return nil, fmt.Errorf("host call %s: %s", method, msg)
	}
	return env.Result, nil
}

// hostAuthJSON returns the credential JSON for an auth index via host.auth.get.
func hostAuthJSON(authIndex string) ([]byte, error) {
	payload, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	result, err := hostCall(pluginabi.MethodHostAuthGet, payload)
	if err != nil {
		return nil, err
	}
	var resp struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}

// hostAuthList returns every credential the host knows via host.auth.list.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	result, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// hostLog forwards a log line to CPA's logger through the host callback.
// CPA prints only the message, not the fields, so they are inlined as well.
func hostLog(level, message string, fields map[string]any) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := json.Marshal(fields[k])
		message += " " + k + "=" + string(v)
	}
	payload, err := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, payload)
}
