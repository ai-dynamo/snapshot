REPO_HACK_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))../hack)
include $(REPO_HACK_DIR)/tools.mk

.DEFAULT_GOAL := generate

CRD_TEMPLATES := ../charts/snapshot/templates/crd

.PHONY: generate fmt lint tidy test

generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object:headerFile=../hack/boilerplate.go.txt paths=./...
	$(CONTROLLER_GEN) crd paths=./... output:crd:artifacts:config=$(CRD_TEMPLATES)
	@set -eu; for f in $(CRD_TEMPLATES)/*.yaml; do \
		[ -e "$$f" ] || continue; \
		{ echo '{{- if .Values.crds.enabled }}' && \
		  awk '/^  name:/ && !l {print "  labels:"; print "    {{- include \"snapshot.labels\" . | nindent 4 }}"; l=1} {print} /^  annotations:$$/ && !d {print "    {{- if .Values.crds.keep }}"; print "    helm.sh/resource-policy: keep"; print "    {{- end }}"; d=1} END {if (!d || !l) exit 3}' "$$f" && \
		  echo '{{- end }}'; } > "$$f.tmp" || { rm -f "$$f.tmp"; exit 1; }; \
		mv "$$f.tmp" "$$f"; \
	done

fmt:
	gofmt -w .

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...

tidy:
	go mod tidy

test:
	go test ./...
