// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// A glibc resolver adapter, not an ELF loader. The Rust sibling owns CUDA state.
#define _GNU_SOURCE
#include "core_abi.h"
#include <dlfcn.h>
#include <link.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define API __attribute__((visibility("default")))
enum { SUCCESS = 0, INVALID_VALUE = 1, NOT_INITIALIZED = 3, NOT_SUPPORTED = 801 };

API const struct { uint32_t cuda_version, protocol_version; } cuinterpose_build_info = {CUDA_VERSION, 4};
static pthread_once_t resolver_once = PTHREAD_ONCE_INIT;
static void *(*real_dlsym)(void *, const char *);
static _Atomic(const struct Core *) core_api;
static atomic_bool initializing, failed, core_unavailable;
static int origin_pid;

// Published nodes and their dlopen references live until exit. Readers need no
// lock, including after quiescent fork; no loader call runs under a shim mutex.
struct Provider {
    void *handle;
    struct Provider *next;
};
static _Atomic(struct Provider *) providers;

static void initialize_resolver(void) {
    real_dlsym = (void *(*)(void *, const char *))dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
}

static void *lookup(void *handle, const char *name) {
    if (pthread_once(&resolver_once, initialize_resolver) != 0 || !real_dlsym)
        return NULL;
    return real_dlsym(handle, name);
}

static int provider_family(const char *path) {
    const char *base = strrchr(path, '/');
    base = base ? base + 1 : path;
    const char *names[] = {"libcuda.so", "libcudart.so"};
    for (unsigned i = 0; i < 2; ++i) {
        size_t length = strlen(names[i]);
        if (strncmp(base, names[i], length) != 0)
            continue;
        const char *suffix = base + length;
        while (*suffix == '.') {
            ++suffix;
            if (*suffix < '0' || *suffix > '9')
                return 0;
            while (*suffix >= '0' && *suffix <= '9')
                ++suffix;
        }
        if (!*suffix)
            return (int)i + 1;
    }
    return 0;
}

// Qualify explicit CUDA handles before substitution; retain the actual provider
// so an application dlclose cannot invalidate cached driver pointers.
static bool retain_provider(void *selected, void *address) {
    Dl_info info;
    if (!address || !dladdr(address, &info))
        return false;
    int family = provider_family(info.dli_fname);
    if (!family)
        return false;
    if (selected != RTLD_DEFAULT && selected != RTLD_NEXT) {
        struct link_map *map = NULL;
        Lmid_t namespace;
        if (dlinfo(selected, RTLD_DI_LINKMAP, &map) != 0 || !map ||
            dlinfo(selected, RTLD_DI_LMID, &namespace) != 0 || namespace != LM_ID_BASE ||
            provider_family(map->l_name) != family)
            return false;
    }
    void *handle = dlopen(info.dli_fname, RTLD_LAZY | RTLD_NOLOAD);
    if (!handle) {
        atomic_store(&failed, true);
        return false;
    }
    struct link_map *map = NULL;
    if (dlinfo(handle, RTLD_DI_LINKMAP, &map) != 0 || !map ||
        (void *)map->l_addr != info.dli_fbase) {
        dlclose(handle);
        return false;
    }
    struct Provider *head = atomic_load(&providers);
    for (struct Provider *node = head; node; node = node->next) {
        if (node->handle == handle) {
            dlclose(handle);
            return true;
        }
    }
    struct Provider *node = malloc(sizeof(*node));
    if (!node) {
        dlclose(handle);
        atomic_store(&failed, true);
        return false;
    }
    node->handle = handle;
    do {
        node->next = head;
    } while (!atomic_compare_exchange_weak(&providers, &head, node));
    return true;
}

static void *resolve(const char *name) {
    if (atomic_load(&failed))
        return NULL;
    void *address = lookup(RTLD_NEXT, name);
    if (!address) {
        for (struct Provider *node = atomic_load(&providers); node; node = node->next) {
            address = lookup(node->handle, name);
            if (address)
                break;
        }
    }
    if (address) {
        retain_provider(RTLD_DEFAULT, address);
        return atomic_load(&failed) ? NULL : address;
    }
    // Runtime-private initialization can load CUDA without intercepted dlsym.
    void *handle = dlopen(strncmp(name, "cuda", 4) == 0 ? "libcudart.so.13" : "libcuda.so.1",
                          RTLD_LAZY | RTLD_LOCAL);
    if (!handle)
        return NULL;
    address = lookup(handle, name);
    if (!retain_provider(handle, address))
        address = NULL;
    dlclose(handle);
    return address;
}

static const struct Core *load_core(void) {
    Dl_info info;
    if (!dladdr((void *)load_core, &info))
        return NULL;
    const char *slash = strrchr(info.dli_fname, '/');
    size_t prefix = slash ? (size_t)(slash - info.dli_fname + 1) : 0;
    const char sibling[] = "libcuinterpose_core.so";
    char *path = malloc(prefix + sizeof(sibling));
    if (!path)
        return NULL;
    memcpy(path, info.dli_fname, prefix);
    memcpy(path + prefix, sibling, sizeof(sibling));
    void *library = dlopen(path, RTLD_LAZY | RTLD_LOCAL);
    free(path);
    if (!library)
        return NULL;
    int (*initialize)(const struct Host *, const struct Core **) =
        lookup(library, "cuinterpose_core_init");
    if (!initialize) {
        dlclose(library);
        return NULL;
    }
    struct Host host = {ABI_VERSION, sizeof(host), resolve, origin_pid};
    const struct Core *api = NULL;
    // Initialization may start Rust workers even before reporting failure.
    // Once called, retain the library on both outcomes rather than unloading
    // code while a failed-startup worker finishes its asynchronous cleanup.
    if (initialize(&host, &api) != SUCCESS || !api ||
        api->version != ABI_VERSION || api->size != sizeof(*api))
        return NULL;
    // Keep the successful dlopen reference: callbacks and Rust threads outlive
    // the initializing call. Matching version/size promises a valid full table.
    return api;
}

static const struct Core *core(void) {
    if (atomic_load(&failed) || atomic_load(&core_unavailable))
        return NULL;
    const struct Core *api = atomic_load(&core_api);
    if (api)
        return api;
    // A constructor may own the loader lock while another thread initializes.
    // Reentry must return transient not-initialized rather than wait on it.
    bool expected = false;
    if (!atomic_compare_exchange_strong(&initializing, &expected, true))
        return NULL;
    api = atomic_load(&core_api);
    if (!api) {
        api = load_core();
        if (api)
            atomic_store(&core_api, api);
        else
            atomic_store(&core_unavailable, true);
    }
    atomic_store(&initializing, false);
    return api;
}

static void fork_prepare(void) {
    const struct Core *api = atomic_load(&core_api);
    if (api)
        api->fork_prepare();
}
static void fork_parent(void) {
    const struct Core *api = atomic_load(&core_api);
    if (api)
        api->fork_parent();
}
static void fork_child(void) {
    const struct Core *api = atomic_load(&core_api);
    if (api)
        api->fork_child();
}
__attribute__((constructor)) static void initialize_process(void) {
    origin_pid = getpid();
    if (pthread_atfork(fork_prepare, fork_parent, fork_child) != 0)
        atomic_store(&failed, true);
}

#define MEMORY_API(X) \
    X(cuMemAlloc_v2, (uint64_t *out, size_t size), (out, size)) \
    X(cuMemFree_v2, (uint64_t address), (address)) \
    X(cuMemGetAddressRange_v2, (uint64_t *base, size_t *size, uint64_t address), (base, size, address)) \
    X(cuIpcGetMemHandle, (struct IpcMemHandle *out, uint64_t address), (out, address)) \
    X(cuIpcOpenMemHandle, (uint64_t *out, struct IpcMemHandle handle, uint32_t flags), (out, handle, flags)) \
    X(cuIpcOpenMemHandle_v2, (uint64_t *out, struct IpcMemHandle handle, uint32_t flags), (out, handle, flags)) \
    X(cuIpcCloseMemHandle, (uint64_t address), (address)) \
    X(cuMemCreate, (uint64_t *out, size_t size, const struct AllocationProp *prop, uint64_t flags), (out, size, prop, flags)) \
    X(cuMemGetAllocationGranularity, (size_t *out, const struct AllocationProp *prop, uint32_t flags), (out, prop, flags)) \
    X(cuMemRelease, (uint64_t handle), (handle)) \
    X(cuMemRetainAllocationHandle, (uint64_t *out, void *address), (out, address)) \
    X(cuMemMap, (uint64_t address, size_t size, size_t offset, uint64_t handle, uint64_t flags), (address, size, offset, handle, flags)) \
    X(cuMemUnmap, (uint64_t address, size_t size), (address, size)) \
    X(cuMemSetAccess, (uint64_t address, size_t size, const struct Access *access, size_t count), (address, size, access, count)) \
    X(cuMemExportToShareableHandle, (void *out, uint64_t handle, uint32_t kind, uint64_t flags), (out, handle, kind, flags)) \
    X(cuMemImportFromShareableHandle, (uint64_t *out, void *fd, uint32_t kind), (out, fd, kind)) \
    X(cuMemGetAllocationPropertiesFromHandle, (struct AllocationProp *out, uint64_t handle), (out, handle)) \
    X(cuMulticastCreate, (uint64_t *out, const struct MulticastProp *prop), (out, prop)) \
    X(cuMulticastAddDevice, (uint64_t handle, int32_t device), (handle, device)) \
    X(cuMulticastBindMem, (uint64_t handle, size_t offset, uint64_t member, size_t member_offset, size_t size, uint64_t flags), (handle, offset, member, member_offset, size, flags)) \
    X(cuMulticastBindMem_v2, (uint64_t handle, int32_t device, size_t offset, uint64_t member, size_t member_offset, size_t size, uint64_t flags), (handle, device, offset, member, member_offset, size, flags)) \
    X(cuMulticastBindAddr, (uint64_t handle, size_t offset, uint64_t address, size_t size, uint64_t flags), (handle, offset, address, size, flags)) \
    X(cuMulticastBindAddr_v2, (uint64_t handle, int32_t device, size_t offset, uint64_t address, size_t size, uint64_t flags), (handle, device, offset, address, size, flags)) \
    X(cuMulticastGetGranularity, (size_t *out, const struct MulticastProp *prop, uint32_t flags), (out, prop, flags)) \
    X(cuMulticastUnbind, (uint64_t handle, int32_t device, size_t offset, size_t size), (handle, device, offset, size))

// The generated Rust table is authoritative. Check the adapters' full pointer
// types, not just call compatibility (which permits implicit integer casts).
#define WRAPPER(name, parameters, arguments) \
    API int name parameters { \
        const struct Core *api = core(); \
        return api ? api->name arguments : NOT_INITIALIZED; \
    } \
    _Static_assert(__builtin_types_compatible_p(__typeof__(&name), __typeof__(((struct Core *)0)->name)), \
                   #name " callback signature");
MEMORY_API(WRAPPER)
#undef WRAPPER

API int cuInit(unsigned flags);
API int cuGetProcAddress(const char *, void **, int, uint64_t);
API int cuGetProcAddress_v2(const char *, void **, int, uint64_t, int *);
API int cuGetProcAddress_v2_ptsz(const char *, void **, int, uint64_t, int *);
API int cudaGetDriverEntryPoint(const char *, void **, uint64_t, int *);
API int cudaGetDriverEntryPoint_ptsz(const char *, void **, uint64_t, int *);
API int cudaGetDriverEntryPointByVersion(const char *, void **, unsigned, uint64_t, int *);
API int cudaGetDriverEntryPointByVersion_ptsz(const char *, void **, unsigned, uint64_t, int *);

static void *replacement(const char *name) {
#define ENTRY(function) if (strcmp(name, #function) == 0) return (void *)function;
#define MEMORY_ENTRY(function, parameters, arguments) ENTRY(function)
    MEMORY_API(MEMORY_ENTRY)
#undef MEMORY_ENTRY
    ENTRY(cuInit)
    ENTRY(cuGetProcAddress)
    ENTRY(cuGetProcAddress_v2)
    ENTRY(cuGetProcAddress_v2_ptsz)
    ENTRY(cudaGetDriverEntryPoint)
    ENTRY(cudaGetDriverEntryPoint_ptsz)
    ENTRY(cudaGetDriverEntryPointByVersion)
    ENTRY(cudaGetDriverEntryPointByVersion_ptsz)
#undef ENTRY
    return NULL;
}

API void *dlsym(void *handle, const char *name) {
    void *address = lookup(handle, name);
    if (!address || !retain_provider(handle, address))
        return atomic_load(&failed) ? NULL : address;
    void *wrapper = replacement(name);
    return wrapper ? wrapper : address;
}

static int finish_query(const char *name, void **output) {
    if (!name || !output)
        return INVALID_VALUE;
    /* Query aliases select the ABI from the returned symbol. Do not redirect
       direct dlsym of the legacy 32-bit cuMemAlloc ABI to a 64-bit wrapper. */
    if (replacement(name) || strcmp(name, "cuMemAlloc") == 0 ||
        strcmp(name, "cuMemFree") == 0 || strcmp(name, "cuMemGetAddressRange") == 0) {
        Dl_info info;
        void *wrapper = NULL;
        if (*output && dladdr(*output, &info) && info.dli_sname && info.dli_saddr == *output) {
            const char *actual = info.dli_sname;
            bool same = strcmp(name, actual) == 0 ||
                (strcmp(name, "cuIpcOpenMemHandle") == 0 && strcmp(actual, "cuIpcOpenMemHandle_v2") == 0) ||
                (strcmp(name, "cuMemAlloc") == 0 && strcmp(actual, "cuMemAlloc_v2") == 0) ||
                (strcmp(name, "cuMemFree") == 0 && strcmp(actual, "cuMemFree_v2") == 0) ||
                (strcmp(name, "cuMemGetAddressRange") == 0 && strcmp(actual, "cuMemGetAddressRange_v2") == 0) ||
                (strcmp(name, "cuGetProcAddress") == 0 &&
                 (strcmp(actual, "cuGetProcAddress_v2") == 0 || strcmp(actual, "cuGetProcAddress_v2_ptsz") == 0)) ||
                (strcmp(name, "cuGetProcAddress_v2") == 0 && strcmp(actual, "cuGetProcAddress_v2_ptsz") == 0) ||
                (strcmp(name, "cuMulticastBindMem") == 0 && strcmp(actual, "cuMulticastBindMem_v2") == 0) ||
                (strcmp(name, "cuMulticastBindAddr") == 0 && strcmp(actual, "cuMulticastBindAddr_v2") == 0);
            void *candidate = same ? replacement(actual) : NULL;
            if (candidate && (*output == candidate || retain_provider(RTLD_DEFAULT, *output)))
                wrapper = candidate;
        }
        if (!wrapper) {
            *output = NULL;
            return NOT_SUPPORTED;
        }
        *output = wrapper;
    }
    const struct Core *api = core();
    int result = api ? api->ensure_ready() : NOT_INITIALIZED;
    if (result != SUCCESS)
        *output = NULL;
    return result;
}

API int cuInit(unsigned flags) {
    int (*function)(unsigned) = resolve("cuInit");
    if (!function)
        return NOT_INITIALIZED;
    int result = function(flags);
    if (result != SUCCESS)
        return result;
    const struct Core *api = core();
    return api ? api->ensure_ready() : NOT_INITIALIZED;
}

API int cuGetProcAddress(const char *name, void **out, int version, uint64_t flags) {
    int (*function)(const char *, void **, int, uint64_t) = resolve("cuGetProcAddress");
    if (!function)
        return NOT_INITIALIZED;
    int result = function(name, out, version, flags);
    return result == SUCCESS ? finish_query(name, out) : result;
}

API int cuGetProcAddress_v2(const char *name, void **out, int version, uint64_t flags, int *status) {
    int (*function)(const char *, void **, int, uint64_t, int *) = resolve("cuGetProcAddress_v2");
    if (!function)
        return NOT_INITIALIZED;
    int result = function(name, out, version, flags, status);
    return result == SUCCESS && (!status || *status == 0) ? finish_query(name, out) : result;
}

API int cuGetProcAddress_v2_ptsz(const char *name, void **out, int version, uint64_t flags, int *status) {
    if (!(flags & 3))
        flags |= 2;
    return cuGetProcAddress_v2(name, out, version, flags, status);
}

#define RUNTIME_QUERY(function_name, version_parameter, version_argument) \
    API int function_name(const char *name, void **out, version_parameter uint64_t flags, int *status) { \
        int (*function)(const char *, void **, version_parameter uint64_t, int *) = resolve(#function_name); \
        if (!function) return NOT_INITIALIZED; \
        int result = function(name, out, version_argument flags, status); \
        return result == SUCCESS && (!status || *status == 0) ? finish_query(name, out) : result; \
    }
#define VERSION_PARAMETER unsigned version,
#define VERSION_ARGUMENT version,
RUNTIME_QUERY(cudaGetDriverEntryPoint, , )
RUNTIME_QUERY(cudaGetDriverEntryPoint_ptsz, , )
RUNTIME_QUERY(cudaGetDriverEntryPointByVersion, VERSION_PARAMETER, VERSION_ARGUMENT)
RUNTIME_QUERY(cudaGetDriverEntryPointByVersion_ptsz, VERSION_PARAMETER, VERSION_ARGUMENT)
#undef VERSION_PARAMETER
#undef VERSION_ARGUMENT
#undef RUNTIME_QUERY

API void cuinterpose_debug_stats(struct DebugStats *out) {
    const struct Core *api = core();
    if (out && api)
        api->debug_stats(out);
}
