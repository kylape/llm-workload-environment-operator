.PHONY: build test generate-chart test-chart

build:
	GOMAXPROCS=2 GOFLAGS=-p=1 CGO_ENABLED=0 go build -o bin/manager ./cmd/manager

test:
	GOMAXPROCS=2 GOFLAGS=-p=1 go test ./...

# Refresh packaged CRD/RBAC after changing their canonical config/ manifests.
generate-chart:
	python3 tools/sync-chart-manifests.py

# Requires Helm 3 and tools/requirements-chart.txt; no cluster is contacted.
test-chart:
	python3 tools/sync-chart-manifests.py --check
	helm lint charts/llm-workload-environment-operator --strict
	python3 -B -m unittest discover -s tools -p 'test_helm_chart.py' -v
