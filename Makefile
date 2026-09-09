# Convenience targets. Everything here is a thin wrapper over go/npm.
DATABASE_URL ?= postgres://postgres@localhost:5432/lunex?sslmode=disable
TEST_DATABASE_URL ?= postgres://postgres@localhost:5432/lunex_test?sslmode=disable

.PHONY: build test test-integration migrate-up migrate-down migrate-version run fmt vet frontend-dev frontend-build

build:
	cd backend && go build ./...

test:
	cd backend && go test ./...

# Runs the same suite with the database-backed tests enabled.
test-integration:
	cd backend && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./... -count=1

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
