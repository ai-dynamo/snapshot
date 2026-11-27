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
     mounted at, shared by the PageBroker and operator containers. Nothing is
     mounted when the Secret is not set. */}}
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

{{/* Non-empty when S3 credentials come from storage.s3.credentialEndpoint. */}}
{{- define "snapshot.s3CredentialEndpointEnabled" -}}
{{- if and (eq .Values.storage.type "s3") .Values.storage.s3.credentialEndpoint.url -}}true{{- end -}}
{{- end -}}

{{/* The certificate file PageBroker's image trusts by default. The reader used
     on restore applies storage.s3.caBundleConfigMapRef to its S3 requests only
     and takes the trust for its credential request from this file, so the
     bundle is also mounted here when a credential endpoint is set. */}}
{{- define "snapshot.pageBrokerSystemCABundlePath" -}}/etc/ssl/certs/ca-certificates.crt{{- end -}}

{{/* Directory the projected service-account token for
     storage.s3.credentialEndpoint is mounted at, in PageBroker and the operator. */}}
{{- define "snapshot.s3IdentityTokenMountDir" -}}/var/run/secrets/snapshot/s3-identity{{- end -}}

{{/* Variables the S3 clients' default credential chain reads to present the
     projected token to the credential endpoint. */}}
{{- define "snapshot.s3CredentialEndpointEnv" -}}
- name: AWS_CONTAINER_CREDENTIALS_FULL_URI
  value: {{ .Values.storage.s3.credentialEndpoint.url | quote }}
- name: AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE
  value: {{ printf "%s/token" (include "snapshot.s3IdentityTokenMountDir" .) | quote }}
{{- end -}}

{{- define "snapshot.s3IdentityTokenVolumeMount" -}}
- name: s3-identity-token
  mountPath: {{ include "snapshot.s3IdentityTokenMountDir" . }}
  readOnly: true
{{- end -}}

{{/* kubelet rotates the projected token before it expires, and the clients
     read the file again on every credential refresh. */}}
{{- define "snapshot.s3IdentityTokenVolume" -}}
- name: s3-identity-token
  projected:
    sources:
      - serviceAccountToken:
          path: token
          expirationSeconds: {{ .Values.storage.s3.credentialEndpoint.tokenExpirationSeconds | int }}
          audience: {{ .Values.storage.s3.credentialEndpoint.tokenAudience | quote }}
{{- end -}}

{{/* Fails the render on S3 store settings the operator refuses at startup
     (api/storage.S3), after helm install has reported success. The checks
     follow that code and refuse nothing it accepts; the operator enforces the
     rest. A bucket or prefix that Helm read as a number fails too: 0123 in a
     values file arrives as 83, a long number is printed with an exponent, and
     a prefix changed that way names another store. */}}
{{- define "snapshot.validateS3Store" -}}
{{- $s3 := .Values.storage.s3 -}}
{{- range $name := list "bucket" "prefix" -}}
{{- $value := index $s3 $name -}}
{{- if not (or (kindIs "string" $value) (kindIs "invalid" $value)) -}}
{{- $read := toJson $value -}}
{{- if or (kindIs "float64" $value) (kindIs "int64" $value) -}}
{{- $read = printf "the number %s" $read -}}
{{- end -}}
{{- $remedy := printf "quote the %s in a values file, or pass it with --set-string" $name -}}
{{- if eq $name "prefix" -}}
{{- $remedy = printf "%s. To keep the snapshots of an installed release, give the prefix that storage.yaml in ConfigMap %s-config has" $remedy (include "snapshot.fullname" $) -}}
{{- end -}}
{{- fail (printf "storage.s3.%s must be a string, but Helm read it as %s: %s" $name $read $remedy) -}}
{{- end -}}
{{- end -}}
{{- $endpoint := $s3.endpoint | default "" | toString -}}
{{- if and $endpoint (not (regexMatch "^[hH][tT][tT][pP][sS]?://[^/?#@:][^/?#@]*[?]?#?$" $endpoint)) -}}
{{- /* The userinfo can hold a password, and this message ends up in CI logs. */ -}}
{{- $shown := regexReplaceAll "^(.*?//)?[^/?#]*@" $endpoint "${1}<userinfo>@" -}}
{{- fail (printf "storage.s3.endpoint must be an absolute http(s) URL with no path, query, fragment or userinfo, but Helm read it as %q: keep only the scheme, the host and an optional port, as in https://s3.example.com" $shown) -}}
{{- end -}}
{{- $prefix := $s3.prefix | default "" -}}
{{- if hasPrefix "/" $prefix -}}
{{- fail (printf "storage.s3.prefix must be a relative key prefix, but Helm read it as %q: remove the leading slash" $prefix) -}}
{{- end -}}
{{- if or (contains "\\" $prefix) (has ".." (splitList "/" $prefix)) (ne (trim $prefix) $prefix) (regexMatch "\\p{Cc}" $prefix) (gt (len $prefix) 1024) -}}
{{- fail (printf "storage.s3.prefix must be a relative key prefix of at most 1024 bytes, with no backslash, control character, \"..\" component or surrounding space, but Helm read it as %q: remove what is not allowed" $prefix) -}}
{{- end -}}
{{- end -}}

{{/* Fails the render on S3 settings PageBroker or the operator would refuse at startup. */}}
{{- define "snapshot.validateS3Settings" -}}
{{- if eq .Values.storage.type "s3" -}}
{{- $s3 := .Values.storage.s3 -}}
{{- include "snapshot.validateS3Store" . -}}
{{- if $s3.credentialEndpoint.url -}}
{{- if $s3.credentialsSecretRef -}}
{{- fail "storage.s3.credentialEndpoint.url and storage.s3.credentialsSecretRef are two credential sources; set one" -}}
{{- end -}}
{{- if not (hasPrefix "https://" ($s3.credentialEndpoint.url | lower)) -}}
{{- fail "storage.s3.credentialEndpoint.url must be an https URL" -}}
{{- end -}}
{{- if not $s3.credentialEndpoint.tokenAudience -}}
{{- fail "storage.s3.credentialEndpoint.tokenAudience is required with storage.s3.credentialEndpoint.url: a token without an audience is also valid for the Kubernetes API" -}}
{{- end -}}
{{- if lt ($s3.credentialEndpoint.tokenExpirationSeconds | int) 600 -}}
{{- fail "storage.s3.credentialEndpoint.tokenExpirationSeconds must be at least 600" -}}
{{- end -}}
{{- end -}}
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

{{/*
Fixed mount path of the node cache in the agent and PageBroker containers.
The agent and the privileged mount helper pin the same path.
*/}}
{{- define "snapshot.nodeCacheMountPath" -}}
/node-cache
{{- end -}}

{{/*
Node directory of the node cache. Fails when the cache is enabled without an
absolute, normalized path, so a typo cannot mount an unintended directory.
*/}}
{{- define "snapshot.nodeCacheHostPath" -}}
{{- $path := .Values.storage.nodeCache.path | toString -}}
{{- if or (not (hasPrefix "/" $path)) (ne (clean $path) $path) (eq $path "/") -}}
{{- fail (printf "storage.nodeCache.path must be an absolute path to a directory when storage.nodeCache.enabled is true, got %q" $path) -}}
{{- end -}}
{{- $path -}}
{{- end -}}
