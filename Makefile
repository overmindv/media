LOCAL_BIN := $(CURDIR)/bin
GOLANGCI_LINT := $(LOCAL_BIN)/golangci-lint
GOOSE := $(LOCAL_BIN)/goose
DATABASE_URL ?= postgres://media:media@localhost:5435/media?sslmode=disable

.PHONY: run worker build test lint migrate-up migrate-down tidy

# Запуск внутреннего HTTP API
run:
	go run ./cmd/media-api

# Запуск scan/processing worker
worker:
	go run ./cmd/media-worker

# Сборка обоих процессов
build:
	go build ./cmd/media-api ./cmd/media-worker

# Запуск unit/API тестов с race detector
test:
	go test -race ./...

# Проверка статическим анализатором
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

# Применение миграций
migrate-up: $(GOOSE)
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" up

# Откат последней миграции
migrate-down: $(GOOSE)
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" down

tidy:
	go mod tidy

$(GOLANGCI_LINT):
	GOBIN="$(LOCAL_BIN)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6

$(GOOSE):
	GOBIN="$(LOCAL_BIN)" go install github.com/pressly/goose/v3/cmd/goose@v3.26.0
