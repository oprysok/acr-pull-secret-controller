IMG ?= ghcr.io/oprysok/acr-pull-secret-controller:dev
CHART := charts/acr-pull-secret-controller

.PHONY: test vet build image helm-lint

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
