#!/usr/bin/env bash
set -euo pipefail
controller-gen object:headerFile="../hack/boilerplate.go.txt" paths="./..."
