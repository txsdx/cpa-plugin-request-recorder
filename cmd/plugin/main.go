package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"
import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/recorder"
	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	"github.com/txsdx/cpa-plugin-request-recorder/web"
	"gopkg.in/yaml.v3"
)

var mu sync.RWMutex
var engine *recorder.Engine
var current recorder.Config

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if host == nil || p == nil || host.abi_version != 1 {
		return 1
	}
	p.abi_version = 1
	p.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, out *C.cliproxy_buffer) (code C.int) {
	if out == nil {
		return 1
	}
	out.ptr = nil
	out.len = 0
	defer func() {
		if recover() != nil {
			write(out, []byte(`{"ok":false,"error":{"code":"panic","message":"recorder internal error"}}`))
			code = 1
		}
	}()
	if method == nil || uint64(n) > 512<<20 {
		return 1
	}
	var raw []byte
	if request != nil && n > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	v, err := handle(C.GoString(method), raw)
	var b []byte
	if err != nil {
		code = 1
		b, _ = json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": "recorder_error", "message": err.Error()}})
	} else {
		b, _ = json.Marshal(map[string]any{"ok": true, "result": v})
	}
	write(out, b)
	return code
}
func write(out *C.cliproxy_buffer, b []byte) {
	if out == nil || len(b) == 0 {
		return
	}
	out.ptr = C.CBytes(b)
	out.len = C.size_t(len(b))
}

//export cliproxyPluginFree
func cliproxyPluginFree(p unsafe.Pointer, _ C.size_t) {
	if p != nil {
		C.free(p)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	mu.Lock()
	defer mu.Unlock()
	if engine != nil {
		_ = engine.Close()
		engine = nil
	}
}
func handle(method string, raw []byte) (any, error) {
	if method == "plugin.register" || method == "plugin.reconfigure" {
		var req struct {
			ConfigYAML    []byte `json:"config_yaml"`
			SchemaVersion uint32 `json:"schema_version"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.SchemaVersion < 6 {
			return nil, errors.New("requires CPA plugin schema >= 6 (lifecycle, stream hooks, raw JSON management responses)")
		}
		c := recorder.DefaultConfig()
		if err := yaml.Unmarshal(req.ConfigYAML, &c); err != nil {
			return nil, err
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if engine != nil {
			if c != current {
				return nil, errors.New("recorder configuration changed: restart CPA to apply without losing inflight captures")
			}
			return recorder.Registration(), nil
		}
		var err error
		engine, err = recorder.New(c)
		if err != nil {
			return nil, err
		}
		current = c
		return recorder.Registration(), nil
	}
	if method == "management.register" {
		return recorder.ManagementRegistration(), nil
	}
	if method == "plugin.quiesce" {
		return struct{}{}, nil
	}
	if method == "plugin.shutdown" {
		cliproxyPluginShutdown()
		return struct{}{}, nil
	}
	mu.RLock()
	defer mu.RUnlock()
	if engine == nil {
		return nil, errors.New("recorder not initialized")
	}
	var err error
	switch method {
	case "request.intercept_before", "request.intercept_after":
		var r wire.Request
		if err = json.Unmarshal(raw, &r); err == nil {
			err = engine.Request(r, method == "request.intercept_after")
		}
	case "response.intercept_after", "response.intercept_stream_chunk":
		var r wire.Response
		if err = json.Unmarshal(raw, &r); err == nil {
			err = engine.Response(r, method == "response.intercept_stream_chunk")
		}
	case "request.complete":
		var r wire.Completion
		if err = json.Unmarshal(raw, &r); err == nil {
			err = engine.Complete(r)
		}
	case "usage.handle":
		var r wire.Usage
		if err = json.Unmarshal(raw, &r); err == nil {
			err = engine.Usage(r)
		}
	case "management.handle":
		var r wire.ManagementRequest
		if err = json.Unmarshal(raw, &r); err == nil {
			return engine.Management(r, web.Files), nil
		}
	default:
		return nil, errors.New("unsupported method: " + method)
	}
	return struct{}{}, err
}
