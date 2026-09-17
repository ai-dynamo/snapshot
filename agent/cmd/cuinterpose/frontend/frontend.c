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

// CUDA headers map the source-level legacy spelling to the latest prototype.
// The interposer must still define both ELF symbol spellings independently.
#undef cuGetProcAddress
#undef cuIpcOpenMemHandle

#define API __attribute__((visibility("default")))
static const char GLIBC_DLSYM_VERSION[] = "GLIBC_2.34";

static pthread_once_t resolver_once = PTHREAD_ONCE_INIT;
static void *(*real_dlsym)(void *, const char *);
static _Atomic(const struct BackendAbi *) backend_api;
static atomic_bool failed, backend_unavailable;
static _Thread_local bool loading_backend;
static int origin_pid;

// Published nodes and their dlopen references live until exit. Readers need no
// lock, including after quiescent fork; no loader call runs under a shim mutex.
struct CudaLibrary {
    void *handle;
    struct CudaLibrary *next;
};
static _Atomic(struct CudaLibrary *) cuda_libraries;

static void initialize_resolver(void) {
    real_dlsym =
        (void *(*)(void *, const char *))dlvsym(RTLD_NEXT, "dlsym", GLIBC_DLSYM_VERSION);
}

static void *lookup(void *handle, const char *name) {
    if (pthread_once(&resolver_once, initialize_resolver) != 0 || !real_dlsym)
        return NULL;
    return real_dlsym(handle, name);
}

static int cuda_library_family(const char *path) {
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

// Accept only addresses from libcuda or libcudart in the base loader namespace.
// Keep that CUDA library open so an application dlclose cannot invalidate a
// function pointer cached by the frontend or Rust backend.
static bool retain_cuda_library(void *selected, void *address) {
    Dl_info info;
    if (!address || !dladdr(address, &info))
        return false;
    int family = cuda_library_family(info.dli_fname);
    if (!family)
        return false;
    if (selected != RTLD_DEFAULT && selected != RTLD_NEXT) {
        struct link_map *map = NULL;
        Lmid_t namespace;
        if (dlinfo(selected, RTLD_DI_LINKMAP, &map) != 0 || !map ||
            dlinfo(selected, RTLD_DI_LMID, &namespace) != 0 || namespace != LM_ID_BASE ||
            cuda_library_family(map->l_name) != family)
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
    struct CudaLibrary *head = atomic_load(&cuda_libraries);
    for (struct CudaLibrary *node = head; node; node = node->next) {
        if (node->handle == handle) {
            dlclose(handle);
            return true;
        }
    }
    struct CudaLibrary *node = malloc(sizeof(*node));
    if (!node) {
        dlclose(handle);
        atomic_store(&failed, true);
        return false;
    }
    node->handle = handle;
    do {
        node->next = head;
    } while (!atomic_compare_exchange_weak(&cuda_libraries, &head, node));
    return true;
}

static void *resolve(const char *name) {
    if (atomic_load(&failed))
        return NULL;
    void *address = lookup(RTLD_NEXT, name);
    if (!address) {
        for (struct CudaLibrary *node = atomic_load(&cuda_libraries); node; node = node->next) {
            address = lookup(node->handle, name);
            if (address)
                break;
        }
    }
    if (address) {
        retain_cuda_library(RTLD_DEFAULT, address);
        return atomic_load(&failed) ? NULL : address;
    }
    // Runtime-private initialization can load CUDA without intercepted dlsym.
    void *handle = dlopen(strncmp(name, "cuda", 4) == 0 ? "libcudart.so.13" : "libcuda.so.1",
                          RTLD_LAZY | RTLD_LOCAL);
    if (!handle)
        return NULL;
    address = lookup(handle, name);
    if (!retain_cuda_library(handle, address))
        address = NULL;
    dlclose(handle);
    return address;
}

static const struct BackendAbi *load_backend(void **reference) {
    Dl_info info;
    if (!dladdr((void *)load_backend, &info))
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
    CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **) =
        lookup(library, "cuinterpose_core_init");
    if (!initialize) {
        dlclose(library);
        return NULL;
    }
    struct FrontendAbi frontend = {ABI_VERSION, sizeof(frontend), resolve, origin_pid};
    const struct BackendAbi *api = NULL;
    // The handshake only registers the frontend and returns an immutable table;
    // it must not call back into the loader or start runtime workers.
    if (initialize(&frontend, &api) != CUDA_SUCCESS || !api ||
        api->version != ABI_VERSION || api->size != sizeof(*api)) {
        dlclose(library);
        return NULL;
    }
    *reference = library;
    return api;
}

static const struct BackendAbi *backend(void) {
    if (atomic_load(&failed) || atomic_load(&backend_unavailable))
        return NULL;
    const struct BackendAbi *api = atomic_load(&backend_api);
    if (api)
        return api;
    // Refuse same-thread constructor reentry before dlopen has finished. Other
    // threads may load independently: glibc serializes DSO construction, and
    // the ABI handshake is idempotent. Never wait under a shim lock around dlopen.
    if (loading_backend)
        return NULL;
    loading_backend = true;
    void *reference = NULL;
    api = load_backend(&reference);
    if (api) {
        const struct BackendAbi *expected = NULL;
        // Retain the winner's reference for process-lifetime callbacks/workers.
        // A losing caller owns only an extra reference to that same DSO.
        if (!atomic_compare_exchange_strong(&backend_api, &expected, api)) {
            dlclose(reference);
            api = expected;
        }
    } else {
        atomic_store(&backend_unavailable, true);
    }
    loading_backend = false;
    return api;
}

static void fork_prepare(void) {
    const struct BackendAbi *api = atomic_load(&backend_api);
    if (api)
        api->fork_prepare();
}
static void fork_parent(void) {
    const struct BackendAbi *api = atomic_load(&backend_api);
    if (api)
        api->fork_parent();
}
static void fork_child(void) {
    const struct BackendAbi *api = atomic_load(&backend_api);
    if (api)
        api->fork_child();
}
__attribute__((constructor)) static void initialize_process(void) {
    origin_pid = getpid();
    if (pthread_atfork(fork_prepare, fork_parent, fork_child) != 0)
        atomic_store(&failed, true);
}

#define MEMORY_API(X) \
    X(cuMemAlloc_v2, (CUdeviceptr *out, size_t size), (out, size)) \
    X(cuMemFree_v2, (CUdeviceptr address), (address)) \
    X(cuMemGetAddressRange_v2, (CUdeviceptr *base, size_t *size, CUdeviceptr address), (base, size, address)) \
    X(cuIpcGetMemHandle, (CUipcMemHandle *out, CUdeviceptr address), (out, address)) \
    X(cuIpcOpenMemHandle, (CUdeviceptr *out, CUipcMemHandle handle, unsigned flags), (out, handle, flags)) \
    X(cuIpcOpenMemHandle_v2, (CUdeviceptr *out, CUipcMemHandle handle, unsigned flags), (out, handle, flags)) \
    X(cuIpcCloseMemHandle, (CUdeviceptr address), (address)) \
    X(cuMemCreate, (CUmemGenericAllocationHandle *out, size_t size, const CUmemAllocationProp *prop, unsigned long long flags), (out, size, prop, flags)) \
    X(cuMemGetAllocationGranularity, (size_t *out, const CUmemAllocationProp *prop, CUmemAllocationGranularity_flags flags), (out, prop, flags)) \
    X(cuMemRelease, (CUmemGenericAllocationHandle handle), (handle)) \
    X(cuMemRetainAllocationHandle, (CUmemGenericAllocationHandle *out, void *address), (out, address)) \
    X(cuMemMap, (CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags), (address, size, offset, handle, flags)) \
    X(cuMemUnmap, (CUdeviceptr address, size_t size), (address, size)) \
    X(cuMemSetAccess, (CUdeviceptr address, size_t size, const CUmemAccessDesc *access, size_t count), (address, size, access, count)) \
    X(cuMemExportToShareableHandle, (void *out, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType kind, unsigned long long flags), (out, handle, kind, flags)) \
    X(cuMemImportFromShareableHandle, (CUmemGenericAllocationHandle *out, void *fd, CUmemAllocationHandleType kind), (out, fd, kind)) \
    X(cuMemGetAllocationPropertiesFromHandle, (CUmemAllocationProp *out, CUmemGenericAllocationHandle handle), (out, handle)) \
    X(cuMulticastCreate, (CUmemGenericAllocationHandle *out, const CUmulticastObjectProp *prop), (out, prop)) \
    X(cuMulticastAddDevice, (CUmemGenericAllocationHandle handle, CUdevice device), (handle, device)) \
    X(cuMulticastBindMem, (CUmemGenericAllocationHandle handle, size_t offset, CUmemGenericAllocationHandle member, size_t member_offset, size_t size, unsigned long long flags), (handle, offset, member, member_offset, size, flags)) \
    X(cuMulticastBindMem_v2, (CUmemGenericAllocationHandle handle, CUdevice device, size_t offset, CUmemGenericAllocationHandle member, size_t member_offset, size_t size, unsigned long long flags), (handle, device, offset, member, member_offset, size, flags)) \
    X(cuMulticastBindAddr, (CUmemGenericAllocationHandle handle, size_t offset, CUdeviceptr address, size_t size, unsigned long long flags), (handle, offset, address, size, flags)) \
    X(cuMulticastBindAddr_v2, (CUmemGenericAllocationHandle handle, CUdevice device, size_t offset, CUdeviceptr address, size_t size, unsigned long long flags), (handle, device, offset, address, size, flags)) \
    X(cuMulticastGetGranularity, (size_t *out, const CUmulticastObjectProp *prop, CUmulticastGranularity_flags flags), (out, prop, flags)) \
    X(cuMulticastUnbind, (CUmemGenericAllocationHandle handle, CUdevice device, size_t offset, size_t size), (handle, device, offset, size))

// The generated Rust table is authoritative. Check the adapters' full pointer
// types, not just call compatibility (which permits implicit integer casts).
#define WRAPPER(name, parameters, arguments) \
    API CUresult name parameters { \
        const struct BackendAbi *api = backend(); \
        return api ? api->name arguments : CUDA_ERROR_NOT_INITIALIZED; \
    } \
    _Static_assert(__builtin_types_compatible_p(__typeof__(&name), __typeof__(((struct BackendAbi *)0)->name)), \
                   #name " callback signature");
MEMORY_API(WRAPPER)
#undef WRAPPER

API CUresult cuInit(unsigned flags);
API CUresult cuGetProcAddress(const char *, void **, int, cuuint64_t);
API CUresult cuGetProcAddress_v2(const char *, void **, int, cuuint64_t,
                                CUdriverProcAddressQueryResult *);
API CUresult cuGetProcAddress_v2_ptsz(const char *, void **, int, cuuint64_t,
                                     CUdriverProcAddressQueryResult *);
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
    if (!address || !retain_cuda_library(handle, address))
        return atomic_load(&failed) ? NULL : address;
    void *wrapper = replacement(name);
    return wrapper ? wrapper : address;
}

static CUresult finish_query(const char *name, void **output) {
    if (!name || !output)
        return CUDA_ERROR_INVALID_VALUE;
    // Procedure lookup may return the modern ABI for an unversioned name.
    // Direct dlsym of an old 32-bit allocation ABI must remain untouched.
    if (replacement(name) || strcmp(name, "cuMemAlloc") == 0 ||
        strcmp(name, "cuMemFree") == 0 || strcmp(name, "cuMemGetAddressRange") == 0) {
        Dl_info info;
        void *wrapper = NULL;
        if (*output && dladdr(*output, &info) && info.dli_sname && info.dli_saddr == *output) {
            const char *actual = info.dli_sname;
            bool same = strcmp(name, actual) == 0 ||
                (strcmp(name, "cuMemAlloc") == 0 && strcmp(actual, "cuMemAlloc_v2") == 0) ||
                (strcmp(name, "cuMemFree") == 0 && strcmp(actual, "cuMemFree_v2") == 0) ||
                (strcmp(name, "cuMemGetAddressRange") == 0 && strcmp(actual, "cuMemGetAddressRange_v2") == 0) ||
                (strcmp(name, "cuIpcOpenMemHandle") == 0 && strcmp(actual, "cuIpcOpenMemHandle_v2") == 0) ||
                (strcmp(name, "cuGetProcAddress") == 0 &&
                 (strcmp(actual, "cuGetProcAddress_v2") == 0 || strcmp(actual, "cuGetProcAddress_v2_ptsz") == 0)) ||
                (strcmp(name, "cuGetProcAddress_v2") == 0 && strcmp(actual, "cuGetProcAddress_v2_ptsz") == 0) ||
                (strcmp(name, "cuMulticastBindMem") == 0 && strcmp(actual, "cuMulticastBindMem_v2") == 0) ||
                (strcmp(name, "cuMulticastBindAddr") == 0 && strcmp(actual, "cuMulticastBindAddr_v2") == 0);
            void *candidate = same ? replacement(actual) : NULL;
            if (candidate && (*output == candidate || retain_cuda_library(RTLD_DEFAULT, *output)))
                wrapper = candidate;
        }
        if (!wrapper) {
            *output = NULL;
            return CUDA_ERROR_NOT_SUPPORTED;
        }
        *output = wrapper;
    }
    const struct BackendAbi *api = backend();
    CUresult result = api ? api->ensure_cuinterpose_initialized() : CUDA_ERROR_NOT_INITIALIZED;
    if (result != CUDA_SUCCESS)
        *output = NULL;
    return result;
}

API CUresult cuInit(unsigned flags) {
    CUresult (*function)(unsigned) = resolve("cuInit");
    if (!function)
        return CUDA_ERROR_NOT_INITIALIZED;
    CUresult result = function(flags);
    if (result != CUDA_SUCCESS)
        return result;
    const struct BackendAbi *api = backend();
    return api ? api->ensure_cuinterpose_initialized() : CUDA_ERROR_NOT_INITIALIZED;
}

API CUresult cuGetProcAddress(const char *name, void **out, int version, cuuint64_t flags) {
    CUresult (*function)(const char *, void **, int, cuuint64_t) = resolve("cuGetProcAddress");
    if (!function)
        return CUDA_ERROR_NOT_INITIALIZED;
    CUresult result = function(name, out, version, flags);
    return result == CUDA_SUCCESS ? finish_query(name, out) : result;
}

API CUresult cuGetProcAddress_v2(const char *name, void **out, int version, cuuint64_t flags,
                                CUdriverProcAddressQueryResult *status) {
    CUresult (*function)(const char *, void **, int, cuuint64_t,
                         CUdriverProcAddressQueryResult *) = resolve("cuGetProcAddress_v2");
    if (!function)
        return CUDA_ERROR_NOT_INITIALIZED;
    CUresult result = function(name, out, version, flags, status);
    return result == CUDA_SUCCESS && (!status || *status == 0) ? finish_query(name, out) : result;
}

API CUresult cuGetProcAddress_v2_ptsz(const char *name, void **out, int version,
                                     cuuint64_t flags,
                                     CUdriverProcAddressQueryResult *status) {
    if (!(flags & 3))
        flags |= 2;
    return cuGetProcAddress_v2(name, out, version, flags, status);
}

#define RUNTIME_QUERY(function_name, version_parameter, version_argument) \
    API int function_name(const char *name, void **out, version_parameter uint64_t flags, int *status) { \
        int (*function)(const char *, void **, version_parameter uint64_t, int *) = resolve(#function_name); \
        if (!function) return CUDA_ERROR_NOT_INITIALIZED; \
        int result = function(name, out, version_argument flags, status); \
        return result == CUDA_SUCCESS && (!status || *status == 0) ? (int)finish_query(name, out) : result; \
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
