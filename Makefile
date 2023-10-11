# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:trivialVersions=true,preserveUnknownFields=false"

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

PROJECT_NAME ?= $(shell go list -m | cut -d '/' -f 3)
# PROJECT_NAME := port-scan-exporter

# The version which will be reported by the --version argument of each binary
# and which will be used as the Docker image tag
# VERSION ?= $(shell git describe --tags)
VERSION ?= 0.2
## The Docker repository name, overridden in CI.
DOCKER_REGISTRY ?= docker.io/dsimionato
DOCKER_IMAGE_NAME ?= ${PROJECT_NAME}
## Image URL to use all building/pushing image targets
IMG ?= ${DOCKER_REGISTRY}/${DOCKER_IMAGE_NAME}:${VERSION}

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

# BIN is the directory where tools will be installed
export BIN ?= ${CURDIR}/bin

OS := $(shell go env GOOS)
ARCH := $(shell go env GOARCH)

# Kind
K8S_CLUSTER_NAME := ${PROJECT_NAME}-cluster
KIND_LOGS := ${CURDIR}/kind_exported_logs


all: build

##@ General
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

fmt: ## Run go fmt against code.
	go fmt ./...

vet: ## Run go vet against code.
	go vet ./...

ENVTEST_ASSETS_DIR=$(shell pwd)/testbin
test: fmt vet ## Run tests.

##@ Build

build: fmt vet ## Build exporter binary.
	go build -o bin/port-scan-exporter port-scan-exporter.go

run: fmt vet ## Run a controller from your host.
	go run ./port-scan-exporter.go

docker-build: test ## Build docker image with the exporter.
	docker build -t ${IMG} .
# docker-buildx build --platform linux/amd64,linux/arm64 -t ${IMG} .
# podman build --arch amd64 -t ${IMG} .

docker-push: ## Push docker image with the exporter.
	docker push ${IMG}

##@ Deployment

#TODO: add automation to package helm chart and deploy it to k8s

##@ Testing

.PHONY: kind-cluster
kind-cluster: ## Use Kind to create a Kubernetes cluster for testing
kind-cluster: ${KIND}
	 ${KIND} get clusters | grep ${K8S_CLUSTER_NAME} || ${KIND} create cluster --name ${K8S_CLUSTER_NAME}

.PHONY: kind-load
kind-load: ## Load all the Docker images into Kind
	${KIND} load docker-image --name ${K8S_CLUSTER_NAME} ${IMG}

.PHONY: kind-export-logs
kind-export-logs: ## Export kind logs to kind_exported_logs subdirectory
kind-export-logs:
	${KIND} export logs --name ${K8S_CLUSTER_NAME} ${KIND_LOGS}

########
# Binaries & tools

##@ Build Dependencies

LOCAL_OS := $(shell go env GOOS)
LOCAL_ARCH := $(shell go env GOARCH)

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KIND ?= $(LOCALBIN)/kind

## Tool Versions
KIND_VERSION := 0.20.0

LOCALKIND := $(shell kind --version 2>/dev/null)

.PHONY: kind
kind: $(LOCALBIN) ## Download Kind locally if necessary. 
ifdef LOCALKIND
	ln -s $(shell which kind) $(KIND)
else 
	curl -fsSL -o ${KIND} https://github.com/kubernetes-sigs/kind/releases/download/v${KIND_VERSION}/kind-${LOCAL_OS}-${LOCAL_ARCH}
	chmod +x ${KIND}
endif
