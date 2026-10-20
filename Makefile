include .env.example
-include .env
export

.PHONY: run build test lint sqlc migrate-up migrate-down seed db-up db-down

run:
	go run ./cmd/api

build:
	go build -o bin/api ./cmd/api

test:
	go test ./...

lint:
	go vet ./...

sqlc:
	go tool sqlc generate

# `go tool migrate` is built without DB drivers, so run it with the postgres tag.
MIGRATE = go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate

migrate-up:
	$(MIGRATE) -path migrations -database "$(MIGRATE_DATABASE_URL)" up

migrate-down:
	$(MIGRATE) -path migrations -database "$(MIGRATE_DATABASE_URL)" down 1

# Create/reset the first admin from ADMIN_EMAIL / ADMIN_PASSWORD (runs as the migration owner).
seed:
	SEED_DATABASE_URL="$(MIGRATE_DATABASE_URL)" go run ./cmd/seed

db-up:
	docker compose up -d --wait db

db-down:
	docker compose down
