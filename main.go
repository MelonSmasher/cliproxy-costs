// Command cliproxy-costs is a CLIProxyAPI native plugin (build with
// -buildmode=c-shared). It exports the ABI 1 entry point and dispatches every
// RPC into internal/plugin.
package main

/*
// C ABI declarations copied from CLIProxyAPI examples/plugin/usage/go/main.go
// (Copyright (c) Luis Pater, Router-For.ME; MIT; see THIRD_PARTY_NOTICES.md).
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
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

// The host API table lives in host memory for the lifetime of the plugin; it
// is kept in C so no Go pointer ever crosses the ABI.
static const cliproxy_host_api* costs_host;

static void costs_set_host(const cliproxy_host_api* host) { costs_host = host; }

static int costs_host_call(const char* method, const uint8_t* req, size_t n, cliproxy_buffer* out) {
	if (costs_host == NULL || costs_host->call == NULL) return -1;
	return costs_host->call(costs_host->host_ctx, method, req, n, out);
}

static void costs_host_free(void* ptr, size_t n) {
	if (ptr != NULL && costs_host != NULL && costs_host->free_buffer != NULL) costs_host->free_buffer(ptr, n);
}

static void costs_fill(cliproxy_plugin_api* api) {
	api->abi_version = 1;
	api->call = cliproxyPluginCall;
	api->free_buffer = cliproxyPluginFree;
	api->shutdown = cliproxyPluginShutdown;
}
*/
import "C"

import (
	"fmt"
	"sync/atomic"
	// The dashboard sends its IANA zone (summary?tz=). Windows has no system
	// zoneinfo and CPA hosts carry no Go install, so the database is embedded.
	_ "time/tzdata"
	"unsafe"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/plugin"
)

// version is injected with -ldflags "-X main.version=<semver>".
var version = "0.0.0-dev"

func main() {}

// hostBridge calls back into the host through the C function table. After
// shutdown begins every call fails without touching host memory.
type hostBridge struct{ closed atomic.Bool }

func (h *hostBridge) Call(method string, request []byte) ([]byte, error) {
	if h.closed.Load() {
		return nil, abi.ErrHostClosed
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
	}
	var out C.cliproxy_buffer
	rc := C.costs_host_call(cMethod, (*C.uint8_t)(cReq), C.size_t(len(request)), &out)
	var resp []byte
	if out.ptr != nil {
		resp = C.GoBytes(out.ptr, C.int(out.len))
		C.costs_host_free(out.ptr, out.len)
	}
	if rc != 0 {
		return resp, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return resp, nil
}

var bridge = &hostBridge{}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	C.costs_set_host(host)
	C.costs_fill(api)
	plugin.Init(bridge, version)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	out := plugin.Handle(C.GoString(method), req)
	response.ptr = C.CBytes(out)
	response.len = C.size_t(len(out))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// Host callbacks are closed first: the host tears down the callback
	// instance before invoking this export and frees the table right after.
	bridge.closed.Store(true)
	plugin.Shutdown()
}
