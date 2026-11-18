IMG ?= ghcr.io/cyphra-dev/operator:dev
CONTROLLER_GEN_VERSION ?= v0.23.0
GO = ./hack/go.sh

.PHONY: generate test build docker-build lint-chart e2e

generate: ## Generate deepcopy code, CRDs and RBAC, then sync the Helm chart.
	$(GO) 'go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) && \
		"$$(go env GOPATH)/bin/controller-gen" object:headerFile=hack/boilerplate.go.txt paths=./... && \
		"$$(go env GOPATH)/bin/controller-gen" crd rbac:roleName=cyphra-operator paths=./... \
			output:crd:artifacts:config=config/crd output:rbac:artifacts:config=config/rbac'
	./hack/sync-chart.sh

test: ## Vet and unit tests.
	$(GO) 'go vet ./... && go test ./...'

build:
	$(GO) 'go build ./...'

docker-build:
	docker build -t $(IMG) .

lint-chart:
	helm lint charts/cyphra-operator

e2e: ## End-to-end test in a throwaway kind cluster.
	./test/e2e/run.sh
