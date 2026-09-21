GO ?= go
.PHONY: test test-race fmt-check vet build local-up local-down integration package

test: fmt-check
	$(GO) test -mod=readonly ./...
	$(GO) vet -mod=readonly ./...

test-race:
	$(GO) test -mod=readonly -race ./...

fmt-check:
	@test -z "$$(gofmt -l cmd internal pkg)" || { gofmt -l cmd internal pkg; exit 1; }

vet:
	$(GO) vet -mod=readonly ./...

build:
	docker build --platform linux/amd64 -f deploy/Dockerfile --target runtime -t nakama-agones-local:dev .

local-up:
	python3 scripts/local_cluster.py up

local-down:
	python3 scripts/local_cluster.py stop

integration:
	node scripts/smoke.mjs

package:
	./scripts/package-release.sh
