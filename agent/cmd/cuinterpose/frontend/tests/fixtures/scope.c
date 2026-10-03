// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// RTLD_LOCAL keeps the plugin's dependency private, so only dlsym calls from this
// plugin can find its symbol. run.py disables sibling calls to preserve that caller
// scope.
#define _GNU_SOURCE
#include <dlfcn.h>

#ifdef SCOPE_DEPENDENCY
int fixture_private_symbol(void) { return 42; }
#else
int fixture_private_symbol(void);
// Referencing the dependency keeps it in DT_NEEDED when linking with --as-needed.
int fixture_scope_value(void) { return fixture_private_symbol(); }
void *fixture_scope_default(const char *name) { return dlsym(RTLD_DEFAULT, name); }
void *fixture_scope_next(const char *name) { return dlsym(RTLD_NEXT, name); }
#endif
