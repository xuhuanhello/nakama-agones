GO ?= go
.PHONY: test test-race fmt-check vet build local-up local-down integration package console package-console

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

console:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -mod=readonly -o dist/fleet-console ./cmd/fleet-console
	CGO_ENABLED=0 $(GO) build -trimpath -mod=readonly -o dist/fleet-console-control ./cmd/fleet-console-control

package-console:
	GO="$(GO)" ./scripts/package-console.sh

local-up:
	python3 scripts/local_cluster.py up

local-down:
	python3 scripts/local_cluster.py stop

integration:
	node scripts/smoke.mjs

package:
	./scripts/package-release.sh
