# Convenience targets. Everything here is a thin wrapper over go/npm.
DATABASE_URL ?= postgres://postgres@localhost:5432/lunex?sslmode=disable
TEST_DATABASE_URL ?= postgres://postgres@localhost:5432/lunex_test?sslmode=disable

.PHONY: build test test-integration test-e2e migrate-up migrate-down migrate-version run fmt vet frontend-dev frontend-build

build:
	cd backend && go build ./...

test:
	cd backend && go test ./...

# Runs the same suite with the database-backed tests enabled.
#
# -p 1 is required, not a preference: internal/db and internal/api both
# TRUNCATE users on the one test database, so running their packages
# concurrently makes each one delete the other's rows.
test-integration:
	cd backend && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./... -count=1 -p 1

# End-to-end: boots the API against a throwaway database, uploads a file,
# searches for a phrase from it, asks the assistant about it, then states a fact
# in one conversation and checks a different conversation retrieves and cites
# it. Needs Postgres with pgvector and a running Ollama; see docs/testing.md.
test-e2e:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" ./scripts/e2e.sh

migrate-up:
	cd backend && DATABASE_URL="$(DATABASE_URL)" go run ./cmd/migrate up

migrate-down:
	cd backend && DATABASE_URL="$(DATABASE_URL)" go run ./cmd/migrate down

migrate-version:
	cd backend && DATABASE_URL="$(DATABASE_URL)" go run ./cmd/migrate version

run:
	cd backend && DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api

fmt:
	cd backend && gofmt -w .

vet:
	cd backend && go vet ./...

frontend-dev:
	cd frontend && npm run dev

frontend-build:
	cd frontend && npm run build
