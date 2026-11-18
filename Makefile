IMG ?= ghcr.io/oprysok/acr-pull-secret-controller:dev
CHART := charts/acr-pull-secret-controller

GOLANGCI_LINT_VERSION ?= v2.15.0

.PHONY: test vet lint build image helm-lint

lint:
	docker run --rm -v $(CURDIR):/src -w /src golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run ./...

test:
	go test -race ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -o bin/manager ./cmd/manager

image:
	docker build -t $(IMG) .

helm-lint:
	helm lint $(CHART) --set registries[0]=example.azurecr.io --set azure.clientId=x --set azure.tenantId=y
