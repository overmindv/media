LOCAL_BIN := $(CURDIR)/bin
GOLANGCI_LINT := $(LOCAL_BIN)/golangci-lint
DATABASE_URL ?= postgres://media:media@localhost:5435/media?sslmode=disable

.PHONY: run build test lint migrate-up migrate-down tidy

# Запуск сервиса (API + processing worker)
run:
	go run ./cmd/media

# Сборка бинарника
build:
	go build ./cmd/media

# Запуск unit/API тестов с race detector
test:
	go test -race ./...

# Проверка статическим анализатором
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

# Миграции через parker
migrate-up:
	go run ./cmd/media migrate --dir migrations --dsn "$(DATABASE_URL)" up

# Откат последней миграции
migrate-down:
	go run ./cmd/media migrate --dir migrations --dsn "$(DATABASE_URL)" down

# Статус миграций
migrate-status:
	go run ./cmd/media migrate --dir migrations --dsn "$(DATABASE_URL)" status

tidy:
	go mod tidy

$(GOLANGCI_LINT):
	GOBIN="$(LOCAL_BIN)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6
