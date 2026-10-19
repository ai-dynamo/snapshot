# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

{{/*
Expand the name of the chart.
*/}}
{{- define "snapshot.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "snapshot.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "snapshot.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "snapshot.labels" -}}
helm.sh/chart: {{ include "snapshot.chart" . }}
{{ include "snapshot.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "snapshot.selectorLabels" -}}
app.kubernetes.io/name: {{ include "snapshot.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the agent service account to use
*/}}
{{- define "snapshot.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "snapshot.fullname" . ) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the operator service account.
*/}}
{{- define "snapshot.operatorServiceAccountName" -}}
{{- printf "%s-operator" (include "snapshot.fullname" .) }}
{{- end }}

{{/*
Use the same agent image for the DaemonSet and cuinterpose installer.
*/}}
{{- define "snapshot.agentImage" -}}
{{- printf "%s:%s" .Values.image.agent.repository (.Values.image.agent.tag | default .Chart.AppVersion) -}}
{{- end }}

{{/*
Convert a Kubernetes quantity to bytes. An unparseable value fails rather than
yielding zero, which would silently disable the check below.
*/}}
{{- define "snapshot.quantityBytes" -}}
{{- $q := . | toString -}}
{{- if hasSuffix "Ki" $q }}{{ mulf (trimSuffix "Ki" $q | float64) 1024 | int64 }}
{{- else if hasSuffix "Mi" $q }}{{ mulf (trimSuffix "Mi" $q | float64) 1048576 | int64 }}
{{- else if hasSuffix "Gi" $q }}{{ mulf (trimSuffix "Gi" $q | float64) 1073741824 | int64 }}
{{- else if hasSuffix "Ti" $q }}{{ mulf (trimSuffix "Ti" $q | float64) 1099511627776 | int64 }}
{{- else if hasSuffix "k" $q }}{{ mulf (trimSuffix "k" $q | float64) 1000 | int64 }}
{{- else if hasSuffix "M" $q }}{{ mulf (trimSuffix "M" $q | float64) 1000000 | int64 }}
{{- else if hasSuffix "G" $q }}{{ mulf (trimSuffix "G" $q | float64) 1000000000 | int64 }}
{{- else if hasSuffix "T" $q }}{{ mulf (trimSuffix "T" $q | float64) 1000000000000 | int64 }}
{{- else if regexMatch "^[0-9]+$" $q }}{{ $q }}
{{- else }}{{ fail (printf "cannot parse memory quantity %q" $q) }}
{{- end }}
{{- end }}

{{/*
Refuse a staging cap the agent cannot survive filling. Rationale and sizing
are in docs/operations/storage.md. Called once from daemonset.yaml.
*/}}
{{- define "snapshot.validateStagingHeadroom" -}}
{{- $staging := include "snapshot.quantityBytes" (include "snapshot.stagingSizeLimit" .) | int64 -}}
{{- $limit := (((.Values.daemonset).resources).limits).memory -}}
{{- if $limit }}
{{- $limitBytes := include "snapshot.quantityBytes" $limit | int64 -}}
{{- if ge $staging $limitBytes }}
{{- fail (printf "pageBroker.staging.sizeLimit (%v) must be smaller than daemonset.resources.limits.memory (%v): staging pages are charged to the agent container, so the agent is OOM-killed before the volume fills, taking every capture and restore on the node with it. Lower pageBroker.staging.sizeLimit or raise daemonset.resources.limits.memory, leaving headroom for the agent process and CRIU. See docs/operations/storage.md" (include "snapshot.stagingSizeLimit" .) $limit) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The staging sizeLimit. Falls back even when pageBroker.staging is explicitly
null, as helm upgrade --reuse-values leaves it for a release predating the key:
an absent sizeLimit is an unbounded volume, not a safe default.
*/}}
{{- define "snapshot.stagingSizeLimit" -}}
{{- ((.Values.pageBroker).staging | default dict).sizeLimit | default "56Gi" -}}
{{- end }}

{{/*
Fail fast on unsupported runtime.type values. Called once from daemonset.yaml.
*/}}
{{- define "snapshot.validateRuntime" -}}
{{- if not (has .Values.runtime.type (list "containerd" "crio")) }}
{{- fail (printf "runtime.type must be 'containerd' or 'crio', got %q" .Values.runtime.type) }}
{{- end }}
{{- if and .Values.runtime.storageDir (not (hasPrefix "/" .Values.runtime.storageDir)) }}
{{- fail (printf "runtime.storageDir must be an absolute path, got %q" .Values.runtime.storageDir) }}
{{- end }}
{{- end }}

{{/*
Resolve the runtime socket path. Uses .Values.runtime.socketPath when set,
otherwise falls back to the per-runtime default.
*/}}
{{- define "snapshot.runtimeSocket" -}}
{{- if .Values.runtime.socketPath }}
{{- .Values.runtime.socketPath }}
{{- else if eq .Values.runtime.type "crio" }}
{{- "/var/run/crio/crio.sock" }}
{{- else }}
{{- "/run/containerd/containerd.sock" }}
{{- end }}
{{- end }}

{{/*
Host directory holding per-container storage (overlay upperdirs the agent
reads for rootfs-diff capture, and CRI-O config.json fallback).
*/}}
{{- define "snapshot.runtimeStorageDir" -}}
{{- if .Values.runtime.storageDir }}
{{- .Values.runtime.storageDir }}
{{- else if eq .Values.runtime.type "crio" }}
{{- "/var/lib/containers" }}
{{- else }}
{{- "/var/lib/containerd" }}
{{- end }}
{{- end }}

{{- define "snapshot.pageBrokerControlPath" -}}/pagebroker/control{{- end -}}
{{- define "snapshot.pageBrokerStagingPath" -}}/pagebroker/staging{{- end -}}

{{/* Driver access shared by the agent and PageBroker. */}}
{{- define "snapshot.nvidiaEnvironment" -}}
- name: NVIDIA_VISIBLE_DEVICES
  value: all
- name: NVIDIA_DRIVER_CAPABILITIES
  value: compute,utility
{{- end -}}
