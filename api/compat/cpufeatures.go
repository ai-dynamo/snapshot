// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// instructionSetFeatures are the CPU flags, as /proc/cpuinfo spells them, that
// say a process may execute some instruction. They are CRIU's own
// instruction-level mask, x86_ins_capability_mask in criu/arch/x86/cpu.c, with
// four flags that mask omits although they name instructions - fma, lahf_lm,
// avx512ifma and clwb - and the extensions newer than it added. Three of
// its entries are left out because they name no instruction a process can run:
// rep_good is a kernel performance hint, tme is memory encryption, and monitor
// is MONITOR/MWAIT, which only ring 0 may execute.
var instructionSetFeatures = sets.New(strings.Fields(`
	fpu tsc cx8 sep cmov clflush mmx fxsr sse sse2
	syscall mmxext rdtscp 3dnowext 3dnow nopl
	pni pclmulqdq ssse3 fma cx16 sse4_1 sse4_2 movbe popcnt aes xsave avx f16c rdrand
	lahf_lm abm sse4a misalignsse 3dnowprefetch xop fma4 tbm
	fsgsbase bmi1 hle avx2 bmi2 erms rtm mpx avx512f avx512dq rdseed adx avx512ifma
	clflushopt clwb avx512pf avx512er avx512cd sha_ni avx512bw avx512vl
	xsaveopt xsavec xgetbv1
	avx512vbmi avx512_vbmi2 gfni vaes vpclmulqdq avx512_vnni avx512_bitalg avx512_vpopcntdq rdpid
	clzero avx512_4vnniw avx512_4fmaps
	avx_vnni avx_ifma avx512_bf16 avx512_fp16 avx512_vp2intersect
	amx_bf16 amx_tile amx_int8 amx_fp16 movdiri movdir64b serialize
`)...)

// InstructionSetFeatures keeps the instruction-set flags out of a CPU's flags,
// sorted and without duplicates. The rest describe the machine rather than what
// runs on it - hypervisor, constant_tsc, the vulnerability workarounds - and
// differ between nodes a process moves between freely, so comparing them would
// refuse restores that work. A CPU with none of them is unknown, so the result
// is then nil.
func InstructionSetFeatures(flags []string) []string {
	features := instructionSets(flags)
	if features.Len() == 0 {
		return nil
	}
	return sets.List(features)
}

func instructionSets(flags []string) sets.Set[string] {
	return instructionSetFeatures.Intersection(sets.New(flags...))
}
