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
The agent's affinity: daemonset.affinity with the agent's supported
architectures added to every required node selector term. Terms are ORed, so
the architecture has to be in each one to hold for the whole requirement. An
empty or null term matches no nodes and stays empty.
*/}}
{{- define "snapshot.agentAffinity" -}}
{{- $affinity := deepCopy (.Values.daemonset.affinity | default dict) -}}
{{- $nodeAffinity := $affinity.nodeAffinity | default dict -}}
{{- $required := $nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution | default dict -}}
{{- $architecture := dict "key" "kubernetes.io/arch" "operator" "In" "values" (list "amd64" "arm64") -}}
{{- $terms := list -}}
{{- range $required.nodeSelectorTerms -}}
{{- $term := . | default dict -}}
{{- if or $term.matchExpressions $term.matchFields -}}
{{- $term = set $term "matchExpressions" (prepend ($term.matchExpressions | default list) $architecture) -}}
{{- end -}}
{{- $terms = append $terms $term -}}
{{- end -}}
{{- $_ := set $required "nodeSelectorTerms" ($terms | default (list (dict "matchExpressions" (list $architecture)))) -}}
{{- $_ = set $nodeAffinity "requiredDuringSchedulingIgnoredDuringExecution" $required -}}
{{- $_ = set $affinity "nodeAffinity" $nodeAffinity -}}
{{- toYaml $affinity -}}
{{- end }}

{{/*
Convert a Kubernetes quantity to bytes, or empty when it is not a form we parse.
*/}}
{{- define "snapshot.quantityBytes" -}}
{{- $q := . | toString -}}
{{- if hasSuffix "Ki" $q }}{{ mulf (trimSuffix "Ki" $q | float64) 1024 | int64 }}
{{- else if hasSuffix "Mi" $q }}{{ mulf (trimSuffix "Mi" $q | float64) 1048576 | int64 }}
{{- else if hasSuffix "Gi" $q }}{{ mulf (trimSuffix "Gi" $q | float64) 1073741824 | int64 }}
{{- else if hasSuffix "Ti" $q }}{{ mulf (trimSuffix "Ti" $q | float64) 1099511627776 | int64 }}
{{- else if hasSuffix "Pi" $q }}{{ mulf (trimSuffix "Pi" $q | float64) 1125899906842624 | int64 }}
{{- else if hasSuffix "Ei" $q }}{{ mulf (trimSuffix "Ei" $q | float64) 1152921504606846976 | int64 }}
{{- else if hasSuffix "k" $q }}{{ mulf (trimSuffix "k" $q | float64) 1000 | int64 }}
{{- else if hasSuffix "M" $q }}{{ mulf (trimSuffix "M" $q | float64) 1000000 | int64 }}
{{- else if hasSuffix "G" $q }}{{ mulf (trimSuffix "G" $q | float64) 1000000000 | int64 }}
{{- else if hasSuffix "T" $q }}{{ mulf (trimSuffix "T" $q | float64) 1000000000000 | int64 }}
{{- else if hasSuffix "P" $q }}{{ mulf (trimSuffix "P" $q | float64) 1000000000000000 | int64 }}
{{- else if hasSuffix "E" $q }}{{ mulf (trimSuffix "E" $q | float64) 1000000000000000000 | int64 }}
{{- else if regexMatch "^[0-9]+(\\.[0-9]+)?[eE][+-]?[0-9]+$" $q }}{{ $q | float64 | int64 }}
{{- else if regexMatch "^[0-9]+$" $q }}{{ $q }}
{{- end }}
{{- end }}

{{/*
The staging sizeLimit, lowered to leave the agent room to survive filling it.
Sizing is in docs/operate/configure-storage.md. Falls back when pageBroker.staging is
explicitly null, as helm upgrade --reuse-values leaves it.
*/}}
{{- define "snapshot.stagingSizeLimit" -}}
{{- $configured := ((.Values.pageBroker).staging | default dict).sizeLimit | default "56Gi" -}}
{{- /* Both containers write this volume — the agent on checkpoint, PageBroker
       on staged restore — and the pages charge to whichever wrote them, so the
       smaller limit is the one that has to hold. */ -}}
{{- $limitBytes := "" -}}
{{- range $container := list (((.Values.daemonset).resources).limits).memory (((.Values.pageBroker).resources).limits).memory -}}
{{- if $container -}}
{{- $bytes := include "snapshot.quantityBytes" $container -}}
{{- if $bytes -}}
{{- if or (not $limitBytes) (lt (int64 $bytes) (int64 $limitBytes)) }}{{- $limitBytes = $bytes -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $configuredBytes := include "snapshot.quantityBytes" $configured -}}
{{- if or (not $limitBytes) (not $configuredBytes) -}}
{{- $configured -}}
{{- else -}}
{{- /* Reserve 8Gi for the agent process and CRIU, or an eighth of a smaller limit. */ -}}
{{- $reserve := min (include "snapshot.quantityBytes" "8Gi" | int64) (div (int64 $limitBytes) 8) -}}
{{- $maxBytes := sub (int64 $limitBytes) $reserve -}}
{{- if le (int64 $configuredBytes) $maxBytes }}{{ $configured }}{{ else }}{{ $maxBytes }}{{ end -}}
{{- end -}}
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

{{/* Name of the ConfigMap the operator writes PageBroker's resolved S3
     configuration into (operator/cmd/manager/pagebroker_storage_config.go).
     Referenced from the DaemonSet volume, the operator Deployment's args and
     its RBAC Role's resourceNames: keep all three in sync through this define
     rather than spelling the name out independently in each. */}}
{{- define "snapshot.pageBrokerS3ConfigName" -}}
{{- printf "%s-pagebroker-storage-config" (include "snapshot.fullname" .) -}}
{{- end -}}

{{/* Directory the S3 credentials Secret (storage.s3.credentialsSecretRef) is
     mounted at, shared by the PageBroker and operator containers. */}}
{{- define "snapshot.s3CredentialsMountDir" -}}/etc/snapshot-s3-credentials{{- end -}}

{{/* Directory and file the S3 CA bundle (storage.s3.caBundleConfigMapRef) is
     mounted at, shared by PageBroker and the operator. */}}
{{- define "snapshot.s3CABundleMountDir" -}}/etc/snapshot-s3-ca{{- end -}}
{{- define "snapshot.s3CABundlePath" -}}
{{- printf "%s/%s" (include "snapshot.s3CABundleMountDir" .) (required "storage.s3.caBundleConfigMapKey is required with storage.s3.caBundleConfigMapRef" .Values.storage.s3.caBundleConfigMapKey) -}}
{{- end -}}

{{/* Where PageBroker reads the Model Streamer tuning file (pageBroker.transferEngine.modelStreamer). */}}
{{- define "snapshot.modelStreamerMountDir" -}}/etc/snapshot-model-streamer{{- end -}}

{{/* Rolls the operator and PageBroker when the values behind storage.yaml and
     pagebroker-s3.yaml change, so an upgrade doesn't leave either running on
     stale config. Hashes the raw values rather than including configmap.yaml,
     which would re-run its required()/fail() checks from this context and
     misattribute their errors to the including template. Includes the
     rendered stagingSizeLimit, not just pageBroker.staging.sizeLimit: the
     default stagingBytes is derived from it together with both containers'
     memory limits, so a change to either limit must also roll both. */}}
{{- define "snapshot.operatorConfigChecksum" -}}
{{- toYaml (dict "storage" .Values.storage "pageBrokerS3" .Values.pageBroker.s3 "modelStreamer" .Values.pageBroker.transferEngine.modelStreamer "stagingSizeLimit" (include "snapshot.stagingSizeLimit" .)) | sha256sum -}}
{{- end -}}
{{- define "snapshot.modelStreamerConfigPath" -}}{{ include "snapshot.modelStreamerMountDir" . }}/model-streamer.json{{- end -}}

{{/* PageBroker's S3 limits object: the configured limits, with the staging
     volume cap as stagingBytes unless one was given. Values pass through
     int64 so floats never reach the rendered YAML. */}}
{{- define "snapshot.pageBrokerS3Limits" -}}
{{- $limits := dict -}}
{{- range $key, $value := ((.Values.pageBroker.s3 | default dict).limits | default dict) -}}
{{- $_ := set $limits $key (int64 $value) -}}
{{- end -}}
{{- if not (hasKey $limits "stagingBytes") -}}
{{- $staging := include "snapshot.quantityBytes" (include "snapshot.stagingSizeLimit" .) -}}
{{- if not $staging -}}
{{- fail (printf "pageBroker.staging.sizeLimit %q is not a byte quantity the chart can read" (include "snapshot.stagingSizeLimit" .)) -}}
{{- end -}}
{{- if le (int64 $staging) 0 -}}
{{- fail "pageBroker.staging.sizeLimit must be at least one byte" -}}
{{- end -}}
{{- /* PageBroker rejects a stagingBytes above 1 TiB. */ -}}
{{- $_ := set $limits "stagingBytes" (min (int64 $staging) (int64 1099511627776)) -}}
{{- end -}}
{{- toYaml $limits -}}
{{- end -}}

{{/* Fails the render on S3 settings PageBroker would refuse at startup. */}}
{{- define "snapshot.validateS3Settings" -}}
{{- if eq .Values.storage.type "s3" -}}
{{- $s3 := .Values.storage.s3 -}}
{{- $addressing := $s3.addressing | default "" -}}
{{- if not (has $addressing (list "" "path" "virtual")) -}}
{{- fail (printf "storage.s3.addressing must be \"path\" or \"virtual\", got %q" $addressing) -}}
{{- end -}}
{{- if and (not $s3.allowHttp) (hasPrefix "http://" ($s3.endpoint | default "" | lower)) -}}
{{- fail "storage.s3.endpoint is plain http; set storage.s3.allowHttp=true to accept it" -}}
{{- end -}}
{{- $allowed := list "transactionSeconds" "stagingBytes" "activeTransactions" "uploadPartBytes" "uploadBufferBytes" "uploadActiveFiles" "requestSeconds" "uploadRequestRetries" "uploadConnectSeconds" -}}
{{- $limits := ((.Values.pageBroker.s3 | default dict).limits | default dict) -}}
{{- range $key, $_ := $limits -}}
{{- if not (has $key $allowed) -}}
{{- fail (printf "pageBroker.s3.limits.%s is not a PageBroker S3 limit" $key) -}}
{{- end -}}
{{- end -}}
{{- $activeTransactions := int64 8 -}}
{{- if hasKey $limits "activeTransactions" -}}
{{- $activeTransactions = int64 $limits.activeTransactions -}}
{{- end -}}
{{- if ge $activeTransactions (int64 .Values.pageBroker.maxConcurrentRequests) -}}
{{- fail "pageBroker.s3.limits.activeTransactions must be below pageBroker.maxConcurrentRequests" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Driver access shared by the agent and PageBroker. */}}
{{- define "snapshot.nvidiaEnvironment" -}}
- name: NVIDIA_VISIBLE_DEVICES
  value: all
- name: NVIDIA_DRIVER_CAPABILITIES
  value: compute,utility
{{- end -}}

{{/* The coordinator of pod groups (podGroups.enabled), a container in each
     agent Pod.

     The TLS contract is the mounted files, the same in every mode, so neither
     the coordinator nor the agent knows who made them:
       - the coordinator serves tls.crt and tls.key from the serving Secret,
         mounted at snapshot.coordinatorTLSDir;
       - the agent trusts ca.crt, the trust bundle's key, for the server name.
     By default the operator issues all three (snapshot.coordinatorBuiltInIssuer):
     the CA Secret that only it reads, the serving Secret and the trust bundle
     ConfigMap, in the release's namespace. Setting any coordinator.tls value
     but trust.key brings your own instead, and gives the operator neither the
     issuer's flags nor its RBAC.

     How an agent reaches the coordinator of a group, for the agent's side:
       - Address: tls://<the agent Pod IP of the group's rank-0 member>:<coordinator.port>.
       - Server name: snapshot.coordinatorServerName, passed to the agent as a
         flag.
       - CA: the selected trust bundle's key, the built-in ConfigMap's ca.crt
         or coordinator.tls.trust's key, mounted at
         /etc/snapshot-coordinator/ca/ca.crt.
       - Token: a projected ServiceAccount token for
         snapshot.coordinatorTokenAudience, with expirationSeconds: 3600, at
         /var/run/secrets/snapshot-coordinator/token. The coordinator admits
         it only for members on the agent's own node. */}}

{{/* The name the coordinators' serving certificate carries, which agents
     verify. It is never resolved. */}}
{{- define "snapshot.coordinatorServerName" -}}
{{- .Values.coordinator.tls.serverName | default (printf "%s-coordinator.%s" (include "snapshot.fullname" .) .Release.Namespace) -}}
{{- end -}}

{{/* The audience of the agents' tokens, which the coordinator admits. Its own
     name, so it does not change with coordinator.tls.serverName. */}}
{{- define "snapshot.coordinatorTokenAudience" -}}
{{- printf "%s-coordinator.%s" (include "snapshot.fullname" .) .Release.Namespace -}}
{{- end -}}

{{/* The resources the operator's built-in issuer writes. Referenced from the
     operator's flags, its RBAC Role's resourceNames, and the volumes. */}}
{{- define "snapshot.coordinatorCASecretName" -}}
{{- printf "%s-coordinator-ca" (include "snapshot.fullname" .) -}}
{{- end -}}
{{- define "snapshot.coordinatorTLSSecretName" -}}
{{- printf "%s-coordinator-tls" (include "snapshot.fullname" .) -}}
{{- end -}}
{{- define "snapshot.coordinatorCABundleName" -}}
{{- printf "%s-coordinator-ca-bundle" (include "snapshot.fullname" .) -}}
{{- end -}}

{{/* "true" when the operator issues the coordinator's certificate, empty when
     you bring your own. */}}
{{- define "snapshot.coordinatorBuiltInIssuer" -}}
{{- $tls := .Values.coordinator.tls -}}
{{- if not (or $tls.serving.secretName $tls.trust.configMapName $tls.trust.secretName $tls.serverName) -}}
true
{{- end -}}
{{- end -}}

{{/* The Secret whose tls.crt and tls.key the coordinator serves. */}}
{{- define "snapshot.coordinatorServingSecretName" -}}
{{- .Values.coordinator.tls.serving.secretName | default (include "snapshot.coordinatorTLSSecretName" .) -}}
{{- end -}}

{{/* Where the coordinator reads tls.crt and tls.key. */}}
{{- define "snapshot.coordinatorTLSDir" -}}/etc/snapshot-coordinator/tls{{- end -}}

{{/* Fails the render on a coordinator setting that is mixed or ambiguous.
     Called once from daemonset.yaml, with podGroups.enabled. */}}
{{- define "snapshot.validateCoordinator" -}}
{{- $c := .Values.coordinator -}}
{{- $tls := $c.tls -}}
{{- if eq (int64 $c.port) (int64 $c.healthPort) -}}
{{- fail (printf "coordinator.port and coordinator.healthPort are both %d: the coordinator needs two ports" (int64 $c.port)) -}}
{{- end -}}
{{- if and $tls.trust.configMapName $tls.trust.secretName -}}
{{- fail "coordinator.tls.trust: set configMapName or secretName, not both" -}}
{{- end -}}
{{- $trust := or $tls.trust.configMapName $tls.trust.secretName -}}
{{- if and $tls.serving.secretName (not $trust) -}}
{{- fail "coordinator.tls.serving.secretName needs the CA bundle agents trust: set coordinator.tls.trust.configMapName or coordinator.tls.trust.secretName" -}}
{{- end -}}
{{- if and $trust (not $tls.serving.secretName) -}}
{{- fail "coordinator.tls.trust needs the Secret the coordinator serves: set coordinator.tls.serving.secretName" -}}
{{- end -}}
{{- if and $tls.serverName (not $tls.serving.secretName) -}}
{{- fail "coordinator.tls.serverName names your own serving certificate: set it with coordinator.tls.serving.secretName and coordinator.tls.trust" -}}
{{- end -}}
{{- if and (include "snapshot.coordinatorBuiltInIssuer" .) (ne $tls.trust.key "ca.crt") -}}
{{- fail "coordinator.tls.trust.key names the key of your own trust bundle: set it with coordinator.tls.trust.configMapName or coordinator.tls.trust.secretName" -}}
{{- end -}}
{{- /* One writer per resource: your own resources must not be the ones the
       operator's issuer writes, which an issuer still running, or enabled
       again later, would overwrite. */ -}}
{{- $builtInSecrets := list (include "snapshot.coordinatorCASecretName" .) (include "snapshot.coordinatorTLSSecretName" .) -}}
{{- range $path, $name := dict "coordinator.tls.serving.secretName" $tls.serving.secretName "coordinator.tls.trust.secretName" $tls.trust.secretName -}}
{{- if has $name $builtInSecrets -}}
{{- fail (printf "%s %q is a Secret the operator issues: name your own Secret" $path $name) -}}
{{- end -}}
{{- end -}}
{{- if eq $tls.trust.configMapName (include "snapshot.coordinatorCABundleName" .) -}}
{{- fail (printf "coordinator.tls.trust.configMapName %q is the ConfigMap the operator issues: name your own ConfigMap" $tls.trust.configMapName) -}}
{{- end -}}
{{- if $tls.serverName -}}
{{- if regexMatch "^[0-9]+(\\.[0-9]+){3}$" $tls.serverName -}}
{{- fail (printf "coordinator.tls.serverName %q is an IP address: agents verify a DNS name" $tls.serverName) -}}
{{- end -}}
{{- if or (gt (len $tls.serverName) 253) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $tls.serverName)) -}}
{{- fail (printf "coordinator.tls.serverName %q is not a DNS name" $tls.serverName) -}}
{{- end -}}
{{- end -}}
{{- end -}}
