// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// This plugin uses RTLD_LOCAL and has a private dependency. Only dlsym calls from the
// plugin can find the dependency's symbol. run.py disables sibling calls so dlsym
// returns into this plugin.
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
