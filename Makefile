LOCAL_BIN := $(CURDIR)/bin
GOLANGCI_LINT := $(LOCAL_BIN)/golangci-lint
DATABASE_URL ?= postgres://media:media@localhost:5435/media?sslmode=disable

.PHONY: run build test ctest lint migrate-up migrate-down tidy

# Запуск сервиса (API + processing worker)
run:
	go run ./cmd/media

# Сборка бинарника
build:
	go build ./cmd/media

# Запуск unit/API тестов с race detector
test:
	go test -race ./...

# PostgreSQL component tests require an explicit isolated test database.
ctest:
	@test -n "$(COMPONENT_TEST_DSN)" || (echo "COMPONENT_TEST_DSN is required" >&2; exit 1)
	go test -race -tags=component ./...

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
