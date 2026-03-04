.PHONY: dev build up down logs tidy lint clean

# ── Development ───────────────────────────────────────────────────────────────

## dev: Build image and start in foreground (Ctrl-C to stop).
dev:
	docker compose up --build

## up: Build image and start in background.
up:
	docker compose up --build -d

## down: Stop and remove containers (data volume is preserved).
down:
	docker compose down

## logs: Tail application logs.
logs:
	docker compose logs -f app

# ── Go tooling ────────────────────────────────────────────────────────────────

## tidy: Run go mod tidy inside a temporary Go container (no local Go required).
tidy:
	docker run --rm -v "$$(pwd)":/app -w /app golang:1.22-alpine go mod tidy

## lint: Run staticcheck inside a temporary Go container.
lint:
	docker run --rm -v "$$(pwd)":/app -w /app golang:1.22-alpine \
	  sh -c "go install honnef.co/go/tools/cmd/staticcheck@latest && staticcheck ./..."

# ── Cleanup ───────────────────────────────────────────────────────────────────

## clean: Stop containers and delete the data volume (destructive!).
clean:
	docker compose down -v

help:
	@grep -E '^## ' Makefile | sed 's/^## //'
