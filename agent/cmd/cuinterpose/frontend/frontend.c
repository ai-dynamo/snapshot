// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// A glibc resolver adapter, not an ELF loader. The Rust sibling owns CUDA state.
#define _GNU_SOURCE
#include "core_abi.h"
#include <dlfcn.h>
#include <link.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// dlsym must reach glibc's dlsym through a tail call so that glibc sees the
// application's return address. musttail (GCC 15+, clang 13+) guarantees the
// tail call at every optimization level.
#if !__has_attribute(musttail)
#error "dlsym requires musttail: build the frontend with GCC 15+ or clang 13+"
#endif

#define API __attribute__((visibility("default")))
static const char GLIBC_DLSYM_VERSION[] = "GLIBC_2.34";
static const char CUDA_DRIVER_SONAME[] = "libcuda.so.1";
static const char CUDA_RUNTIME_SONAME[] = "libcudart.so.13";
static const char CUDA_RUNTIME_PREFIX[] = "cuda";
static const char BACKEND_LIBRARY[] = "$ORIGIN/libcuinterpose_core.so";
static const char BACKEND_INIT_SYMBOL[] = "cuinterpose_core_init";
// Library names without version suffixes. Index + 1 is the library family.
static const char *const CUDA_LIBRARY_NAMES[] = {"libcuda.so", "libcudart.so"};
#define LENGTH(array) (sizeof(array) / sizeof((array)[0]))

// cudaError_t and cudaDriverEntryPointQueryResult values returned by the runtime
// wrappers. The frontend build includes only the driver API header.
enum {
    cudaSuccess = 0,
    cudaErrorInitializationError = 3,
    cudaDriverEntryPointSuccess = 0,
    cudaDriverEntryPointSymbolNotFound = 1,
};

// Wrapped memory and owning-context entry points. The backend implements each one.
#define MEMORY_API(X) \
    X(cuCtxDestroy, (CUcontext context), (context)) \
    X(cuCtxDestroy_v2, (CUcontext context), (context)) \
    X(cuDevicePrimaryCtxRelease, (CUdevice device), (device)) \
    X(cuDevicePrimaryCtxRelease_v2, (CUdevice device), (device)) \
    X(cuDevicePrimaryCtxReset, (CUdevice device), (device)) \
    X(cuDevicePrimaryCtxReset_v2, (CUdevice device), (device)) \
    X(cuMemAlloc_v2, (CUdeviceptr *out, size_t size), (out, size)) \
    X(cuMemFree_v2, (CUdeviceptr address), (address)) \
    X(cuMemGetAddressRange_v2, (CUdeviceptr *base, size_t *size, CUdeviceptr address), (base, size, address)) \
    X(cuIpcGetMemHandle, (CUipcMemHandle *out, CUdeviceptr address), (out, address)) \
    X(cuIpcOpenMemHandle, (CUdeviceptr *out, CUipcMemHandle handle, unsigned flags), (out, handle, flags)) \
    X(cuIpcOpenMemHandle_v2, (CUdeviceptr *out, CUipcMemHandle handle, unsigned flags), (out, handle, flags)) \
    X(cuIpcCloseMemHandle, (CUdeviceptr address), (address)) \
    X(cuMemCreate, (CUmemGenericAllocationHandle *out, size_t size, const CUmemAllocationProp *prop, unsigned long long flags), (out, size, prop, flags)) \
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
    X(cuMulticastUnbind, (CUmemGenericAllocationHandle handle, CUdevice device, size_t offset, size_t size), (handle, device, offset, size))


typedef void *(*DlsymFunction)(void *, const char *);
static _Atomic(DlsymFunction) real_dlsym;
static _Atomic(const struct BackendAbi *) backend_api;
static atomic_bool failed, backend_unavailable;
static _Thread_local bool loading_backend;

// Published nodes and their dlopen references live until exit. Readers need no
// lock, including after quiescent fork; no loader call runs under a shim mutex.
struct CudaLibrary {
    void *handle;
    struct CudaLibrary *next;
};
static _Atomic(struct CudaLibrary *) cuda_libraries;

// Finds glibc's dlsym on first use without a once-guard. dlvsym takes the loader
// lock that dlopen holds while running constructors, so waiting for another
// thread's lookup here can deadlock.
static DlsymFunction glibc_dlsym(void) {
    DlsymFunction function = atomic_load(&real_dlsym);
    if (!function) {
        function = (DlsymFunction)dlvsym(RTLD_NEXT, "dlsym", GLIBC_DLSYM_VERSION);
        atomic_store(&real_dlsym, function);
    }
    return function;
}

static void *lookup(void *handle, const char *name) {
    DlsymFunction function = glibc_dlsym();
    return function ? function(handle, name) : NULL;
}

static int cuda_library_family(const char *path) {
    const char *base = strrchr(path, '/');
    base = base ? base + 1 : path;
    for (unsigned i = 0; i < LENGTH(CUDA_LIBRARY_NAMES); ++i) {
        size_t length = strlen(CUDA_LIBRARY_NAMES[i]);
        if (strncmp(base, CUDA_LIBRARY_NAMES[i], length) != 0)
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
enum CudaRetention { CUDA_OUT_OF_SCOPE, CUDA_RETAINED, CUDA_UNAVAILABLE };

static enum CudaRetention retain_cuda_library(void *selected, void *address) {
    Dl_info info;
    if (!address || !dladdr(address, &info))
        return CUDA_OUT_OF_SCOPE;
    int family = cuda_library_family(info.dli_fname);
    if (!family)
        return CUDA_OUT_OF_SCOPE;
    if (selected != RTLD_DEFAULT && selected != RTLD_NEXT) {
        struct link_map *map = NULL;
        Lmid_t namespace;
        if (dlinfo(selected, RTLD_DI_LINKMAP, &map) != 0 || !map ||
            dlinfo(selected, RTLD_DI_LMID, &namespace) != 0 || namespace != LM_ID_BASE ||
            cuda_library_family(map->l_name) != family)
            return CUDA_OUT_OF_SCOPE;
    }
    void *handle = dlopen(info.dli_fname, RTLD_LAZY | RTLD_NOLOAD);
    if (!handle) {
        atomic_store(&failed, true);
        return CUDA_UNAVAILABLE;
    }
    struct link_map *map = NULL;
    if (dlinfo(handle, RTLD_DI_LINKMAP, &map) != 0 || !map ||
        (void *)map->l_addr != info.dli_fbase) {
        dlclose(handle);
        return CUDA_OUT_OF_SCOPE;
    }
    if (atomic_load(&failed)) {
        dlclose(handle);
        return CUDA_UNAVAILABLE;
    }
    struct CudaLibrary *head = atomic_load(&cuda_libraries);
    for (struct CudaLibrary *node = head; node; node = node->next) {
        if (node->handle == handle) {
            dlclose(handle);
            return CUDA_RETAINED;
        }
    }
    struct CudaLibrary *node = malloc(sizeof(*node));
    if (!node) {
        dlclose(handle);
        atomic_store(&failed, true);
        return CUDA_UNAVAILABLE;
    }
    node->handle = handle;
    do {
        node->next = head;
    } while (!atomic_compare_exchange_weak(&cuda_libraries, &head, node));
    return CUDA_RETAINED;
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
    bool runtime = strncmp(name, CUDA_RUNTIME_PREFIX, sizeof(CUDA_RUNTIME_PREFIX) - 1) == 0;
    void *handle = dlopen(runtime ? CUDA_RUNTIME_SONAME : CUDA_DRIVER_SONAME, RTLD_LAZY | RTLD_LOCAL);
    if (!handle)
        return NULL;
    address = lookup(handle, name);
    if (retain_cuda_library(handle, address) != CUDA_RETAINED)
        address = NULL;
    dlclose(handle);
    return address;
}

static const struct BackendAbi *load_backend(void **reference) {
    // glibc expands ORIGIN from this DSO's load-time directory, even after chdir.
    void *library = dlopen(BACKEND_LIBRARY, RTLD_LAZY | RTLD_LOCAL);
    if (!library)
        return NULL;
    CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **) =
        lookup(library, BACKEND_INIT_SYMBOL);
    if (!initialize) {
        dlclose(library);
        return NULL;
    }
    struct FrontendAbi frontend = {ABI_VERSION, sizeof(frontend), resolve};
    const struct BackendAbi *api = NULL;
    // The handshake only registers the frontend and returns an immutable table;
    // it must not call back into the loader or start runtime workers.
    if (initialize(&frontend, &api) != CUDA_SUCCESS || !api ||
        api->version != ABI_VERSION || api->size != sizeof(*api))
        // Retain a rejected backend conservatively: an incompatible library
        // may not honor the handshake-only contract and could have live workers.
        return NULL;
    *reference = library;
    return api;
}

static const struct BackendAbi *backend(void) {
    if (atomic_load(&failed))
        return NULL;
    const struct BackendAbi *api = atomic_load(&backend_api);
    if (api)
        return api;
    if (atomic_load(&backend_unavailable))
        return NULL;
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
        // A failed private load must not hide another caller's published table.
        api = atomic_load(&backend_api);
        if (!api)
            atomic_store(&backend_unavailable, true);
    }
    loading_backend = false;
    return api;
}

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

// The search scope of an explicit handle does not depend on the caller, so the
// frontend can make the lookup itself and substitute a wrapper.
static void *dlsym_handle(void *handle, const char *name) {
    void *address = lookup(handle, name);
    enum CudaRetention retained = retain_cuda_library(handle, address);
    // A broken shim must not break unrelated or out-of-scope loader lookups.
    if (retained == CUDA_OUT_OF_SCOPE)
        return address;
    if (retained == CUDA_UNAVAILABLE || atomic_load(&failed))
        return NULL;
    void *wrapper = replacement(name);
    return wrapper ? wrapper : address;
}

// glibc picks the RTLD_DEFAULT and RTLD_NEXT search scope from its caller's
// return address, so these lookups are tail calls into glibc and their results
// are returned unchanged. RTLD_DEFAULT already finds the preloaded wrappers.
API void *dlsym(void *handle, const char *name) {
    if (handle != RTLD_DEFAULT && handle != RTLD_NEXT)
        return dlsym_handle(handle, name);
    DlsymFunction function = glibc_dlsym();
    if (!function)
        return NULL;
    __attribute__((musttail)) return function(handle, name);
}

// A procedure query for a requested name may return a newer entry point. Old
// names such as cuMemAlloc are listed only here: they have no wrapper of their
// own, and dlsym of their 32-bit ABI must remain untouched.
static const struct {
    const char *requested, *returned;
} QUERY_ALIASES[] = {
    {"cuCtxDestroy", "cuCtxDestroy_v2"},
    {"cuDevicePrimaryCtxRelease", "cuDevicePrimaryCtxRelease_v2"},
    {"cuDevicePrimaryCtxReset", "cuDevicePrimaryCtxReset_v2"},
    {"cuMemAlloc", "cuMemAlloc_v2"},
    {"cuMemFree", "cuMemFree_v2"},
    {"cuMemGetAddressRange", "cuMemGetAddressRange_v2"},
    {"cuIpcOpenMemHandle", "cuIpcOpenMemHandle_v2"},
    {"cuGetProcAddress", "cuGetProcAddress_v2"},
    {"cuGetProcAddress", "cuGetProcAddress_v2_ptsz"},
    {"cuGetProcAddress_v2", "cuGetProcAddress_v2_ptsz"},
    {"cuMulticastBindMem", "cuMulticastBindMem_v2"},
    {"cuMulticastBindAddr", "cuMulticastBindAddr_v2"},
};

static bool query_intercepted(const char *name) {
    if (replacement(name))
        return true;
    for (size_t i = 0; i < LENGTH(QUERY_ALIASES); ++i)
        if (strcmp(name, QUERY_ALIASES[i].requested) == 0)
            return true;
    return false;
}

static bool same_entry_point(const char *requested, const char *returned) {
    if (strcmp(requested, returned) == 0)
        return true;
    for (size_t i = 0; i < LENGTH(QUERY_ALIASES); ++i)
        if (strcmp(requested, QUERY_ALIASES[i].requested) == 0 &&
            strcmp(returned, QUERY_ALIASES[i].returned) == 0)
            return true;
    return false;
}

// Replaces a successful query result with the wrapper for the returned entry
// point. Returns false, with *output cleared, if the result cannot be wrapped.
static bool finish_query(const char *name, void **output) {
    if (!name || !output || !*output || !query_intercepted(name))
        return true;
    Dl_info info;
    void *wrapper = NULL;
    if (dladdr(*output, &info) && info.dli_sname && info.dli_saddr == *output &&
        same_entry_point(name, info.dli_sname))
        wrapper = replacement(info.dli_sname);
    if (!wrapper || (*output != wrapper &&
                     retain_cuda_library(RTLD_DEFAULT, *output) != CUDA_RETAINED)) {
        *output = NULL;
        return false;
    }
    *output = wrapper;
    return true;
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

// A refused result is reported as that API's result for a missing symbol, so
// callers treat the function as unavailable rather than as an error.
API CUresult cuGetProcAddress(const char *name, void **out, int version, cuuint64_t flags) {
    CUresult (*function)(const char *, void **, int, cuuint64_t) = resolve("cuGetProcAddress");
    if (!function)
        return CUDA_ERROR_NOT_INITIALIZED;
    CUresult result = function(name, out, version, flags);
    if (result != CUDA_SUCCESS)
        return result;
    return finish_query(name, out) ? CUDA_SUCCESS : CUDA_ERROR_NOT_FOUND;
}

API CUresult cuGetProcAddress_v2(const char *name, void **out, int version, cuuint64_t flags,
                                CUdriverProcAddressQueryResult *status) {
    CUresult (*function)(const char *, void **, int, cuuint64_t,
                         CUdriverProcAddressQueryResult *) = resolve("cuGetProcAddress_v2");
    if (!function)
        return CUDA_ERROR_NOT_INITIALIZED;
    CUresult result = function(name, out, version, flags, status);
    if (result != CUDA_SUCCESS || (status && *status != CU_GET_PROC_ADDRESS_SUCCESS))
        return result;
    if (!finish_query(name, out) && status)
        *status = CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
    return CUDA_SUCCESS;
}

API CUresult cuGetProcAddress_v2_ptsz(const char *name, void **out, int version,
                                     cuuint64_t flags,
                                     CUdriverProcAddressQueryResult *status) {
    if (!(flags & 3))
        flags |= 2;
    return cuGetProcAddress_v2(name, out, version, flags, status);
}

static bool runtime_query_supported(void *function) {
    Dl_info provider, version_provider;
    if (!dladdr(function, &provider))
        return false;
    void *library = dlopen(provider.dli_fname, RTLD_LAZY | RTLD_NOLOAD);
    if (!library)
        return false;
    int (*get_version)(int *) = lookup(library, "cudaRuntimeGetVersion");
    int version = 0;
    // Handle lookup can find a dependency's symbol. Its version says nothing
    // about the runtime that supplied the query function.
    bool supported = get_version && dladdr((void *)get_version, &version_provider) &&
        version_provider.dli_fbase == provider.dli_fbase &&
        get_version(&version) == cudaSuccess && version >= 12000;
    dlclose(library);
    return supported;
}

#define RUNTIME_QUERY(function_name, version_parameter, version_argument) \
    API int function_name(const char *name, void **out, version_parameter uint64_t flags, int *status) { \
        int (*function)(const char *, void **, version_parameter uint64_t, int *) = resolve(#function_name); \
        /* CUDA 11 callers have no status argument; reject before accessing it. */ \
        if (!function || !runtime_query_supported((void *)function)) \
            return cudaErrorInitializationError; \
        int result = function(name, out, version_argument flags, status); \
        if (result != cudaSuccess || (status && *status != cudaDriverEntryPointSuccess)) \
            return result; \
        if (!finish_query(name, out) && status) \
            *status = cudaDriverEntryPointSymbolNotFound; \
        return cudaSuccess; \
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
