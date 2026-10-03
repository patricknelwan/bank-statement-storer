GO = GOTOOLCHAIN=go1.27.1 go

.PHONY: build test vet vuln generate migrate compose-up
build:
	$(GO) build ./cmd/service
test:
	$(GO) test ./...
vet:
	$(GO) vet ./...
generate:
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
migrate:
	$(GO) run ./cmd/service migrate
compose-up:
	docker compose --env-file .env -f deploy/compose.yml up -d --build
