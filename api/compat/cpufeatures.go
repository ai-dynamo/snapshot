// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// instructionSetFeatures are the CPU flags, as /proc/cpuinfo spells them, that
// say a process may execute some instruction, on either architecture the agent
// ships for. The cpu-arch check refuses a restore across architectures, so a
// name both spell the same, aes, never compares one with the other.
var instructionSetFeatures = sets.New(append(x86InstructionSets, arm64InstructionSets...)...)

// x86InstructionSets are CRIU's own instruction-level mask,
// x86_ins_capability_mask in criu/arch/x86/cpu.c, with four flags that mask
// omits although they name instructions - fma, lahf_lm, avx512ifma and clwb -
// and the extensions newer than it added. Three of its entries are left out
// because they name no instruction a process can run: rep_good is a kernel
// performance hint, tme is memory encryption, and monitor is MONITOR/MWAIT,
// which only ring 0 may execute.
var x86InstructionSets = strings.Fields(`
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
`)

// arm64InstructionSets are the hwcaps the arm64 kernel prints on the Features
// line, hwcap_str in arch/arm64/kernel/cpuinfo.c as of Linux 7.3, which tell a
// process what it may use. The kernel prints only what every processor offers.
// Three are left out: evtstrm says the generic timer sends a periodic event,
// which virtual machines often turn off, rather than that an instruction
// exists, and dgh and rprfm are hints a processor without them runs as NOPs.
var arm64InstructionSets = strings.Fields(`
	fp asimd aes pmull sha1 sha2 crc32 atomics fphp asimdhp cpuid asimdrdm
	jscvt fcma lrcpc dcpop sha3 sm3 sm4 asimddp sha512 sve asimdfhm dit uscat
	ilrcpc flagm ssbs sb paca pacg gcs ls64 dcpodp
	sve2 sveaes svepmull svebitperm svesha3 svesm4 flagm2 frint
	svei8mm svef32mm svef64mm svebf16 i8mm bf16 rng bti mte
	ecv afp rpres mte3 sme smei16i64 smef64f64 smei8i32 smef16f32 smeb16f32
	smef32f32 smefa64 wfxt ebf16 sveebf16 cssc sve2p1 sme2 sme2p1
	smei16i32 smebi32i32 smeb16b16 smef16f16 mops hbc sveb16b16 lrcpc3 lse128
	fpmr lut faminmax f8cvt f8fma f8dp4 f8dp2 f8e4m3 f8e5m2 smelutv2
	smef8f16 smef8f32 smesf8fma smesf8dp4 smesf8dp2 poe
	cmpbr fprcvt f8mm8 f8mm4 svef16mm sveeltperm sveaes2 svebfscale sve2p2
	sme2p2 smesbitperm smeaes smesfexpa smestmop smesmop4 mtefar mtestoreonly
	lsfe sveb16mm sve2p3 smelut6 sme2p3 f16mm f16f32dot f16f32mm svelut6
`)

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
