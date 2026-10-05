// Synthetic ABI boundary checks; no network or credentials.
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <dlfcn.h>

typedef struct { void *ptr; size_t len; } buffer;
typedef int (*host_call)(void *, const char *, const uint8_t *, size_t, buffer *);
typedef struct { uint32_t abi_version; void *ctx; host_call call; void (*free_buffer)(void *, size_t); } host_api;
typedef struct { uint32_t abi_version; int (*call)(char *, uint8_t *, size_t, buffer *); void (*free_buffer)(void *, size_t); void (*shutdown)(void); } plugin_api;
typedef int (*init_fn)(host_api *, plugin_api *);
static int fake_call(void *ctx, const char *method, const uint8_t *req, size_t size, buffer *out) {
 (void)ctx; (void)method; (void)req; (void)size;
 const char *value = "{\"ok\":true,\"result\":{}}";
 out->len = strlen(value); out->ptr = malloc(out->len);
 memcpy(out->ptr, value, out->len); return 0;
}
static void release(void *ptr, size_t len) { (void)len; free(ptr); }
#define CHECK(x) do { if (!(x)) { fprintf(stderr, "ABI check failed at line %d\n", __LINE__); return 1; } } while (0)
int main(int argc, char **argv) {
 CHECK(argc == 2);
 void *lib = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
 if (!lib) { fprintf(stderr, "%s\n", dlerror()); return 1; }
 init_fn init = (init_fn)dlsym(lib, "cliproxy_plugin_init"); CHECK(init != NULL);
 host_api host = {1, NULL, fake_call, release}; plugin_api api = {0};
 CHECK(init(NULL, &api) != 0); CHECK(init(&host, NULL) != 0);
 host.abi_version = 2; CHECK(init(&host, &api) != 0); host.abi_version = 1;
 host.call = NULL; CHECK(init(&host, &api) != 0); host.call = fake_call;
 host.free_buffer = NULL; CHECK(init(&host, &api) != 0); host.free_buffer = release;
 CHECK(init(&host, &api) == 0); CHECK(api.abi_version == 1 && api.call && api.shutdown && api.free_buffer);
 buffer out = {(void *)1, 123};
 CHECK(api.call(NULL, NULL, 0, &out) != 0); CHECK(out.ptr == NULL && out.len == 0);
 CHECK(api.call("plugin.register", NULL, 1, &out) != 0);
 CHECK(api.call("plugin.register", (uint8_t *)1, SIZE_MAX, &out) != 0);
 CHECK(api.call("plugin.register", (uint8_t *)1, (128u << 20) + 1, &out) != 0);
 CHECK(api.call("unknown", NULL, 0, NULL) != 0);
 CHECK(api.call("unknown", NULL, 0, &out) == 0);
 CHECK(out.ptr != NULL && out.len > 0 && out.len < 1024);
 CHECK(memmem(out.ptr, out.len, "unknown_method", 14) != NULL);
 api.free_buffer(out.ptr, out.len);
 api.shutdown(); api.shutdown();
 // Go c-shared runtime owns background threads; do not dlclose Go libraries.
 puts("native ABI boundary checks passed");
 return 0;
}
