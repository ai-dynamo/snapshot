// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"fmt"
)

// Step names the part of a checkpoint or restore flow a failure came from. It
// is rendered into conditions and events, logged as a structured key, and the
// troubleshooting guide is organised by it.
type Step string

const (
	StepResolveArtifact       Step = "resolving checkpoint artifact"
	StepReadManifest          Step = "reading checkpoint manifest"
	StepValidateManifest      Step = "validating checkpoint manifest against the restore request"
	StepVerifyCuInterpose     Step = "verifying cuinterpose library identity"
	StepInspectPlaceholder    Step = "inspecting placeholder container"
	StepBuildGPUContext       Step = "building GPU context"
	StepMountBundle           Step = "mounting agent bundle into placeholder"
	StepMountCuInterpose      Step = "mounting cuinterpose into placeholder"
	StepPrepareRestore        Step = "preparing PageBroker restore"
	StepMountRestoreSource    Step = "mounting PageBroker restore source"
	StepRunNSRestore          Step = "running nsrestore"
	StepCheckStagedCheckpoint Step = "checking the staged checkpoint"
	StepPrepareCRIURestore    Step = "preparing CRIU restore"
	StepApplyRootfsDiff       Step = "applying rootfs diff"
	StepPrepareGPURestore     Step = "preparing GPU restore"
	StepRestoreCRIU           Step = "restoring with CRIU"
	StepRestoreCUDA           Step = "restoring CUDA state"
	StepCommitRestore         Step = "committing PageBroker restore"
	StepValidateRestored      Step = "validating restored process"

	StepResolveCheckpointPath Step = "resolving checkpoint artifact path"
	StepInspectSource         Step = "inspecting source container"
	StepPrepareCheckpoint     Step = "preparing PageBroker checkpoint"
	StepInspectCuInterpose    Step = "inspecting cuinterpose"
	StepStageCUDAJobFile      Step = "staging CUDA job file"
	StepConfigureCheckpoint   Step = "configuring CRIU dump and writing checkpoint manifest"
	StepCheckpointGPU         Step = "checkpointing GPU state"
	StepDumpCRIU              Step = "dumping with CRIU"
	StepCaptureOverlay        Step = "capturing rootfs diff"
	StepCommitCheckpoint      Step = "committing PageBroker checkpoint"
)

// StepError carries the step a failure came from. Unwrap keeps the cause
// visible to errors.Is and errors.As.
type StepError struct {
	Step Step
	Err  error
}

func (e *StepError) Error() string { return stepPrefix(e.Step) + e.Err.Error() }

func stepPrefix(step Step) string { return fmt.Sprintf("while %s: ", step) }

func (e *StepError) Unwrap() error { return e.Err }

func failStep(step Step, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{Step: step, Err: err}
}

// failStepUnlessNamed wraps err with step only when err does not already
// name one, so a step reported from inside nsrestore is kept as is.
func failStepUnlessNamed(step Step, err error) error {
	if StepOf(err) != "" {
		return err
	}
	return failStep(step, err)
}

// StepOf returns the step a failure came from, or "" when it carries none.
func StepOf(err error) Step {
	var stepErr *StepError
	if errors.As(err, &stepErr) {
		return stepErr.Step
	}
	return ""
}
