# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`media` — владелец файлов и изображений Overmindv. PostgreSQL хранит только метаданные и жизненный цикл, бинарные данные лежат в S3-совместимом object storage (MinIO). Это часть монорепозитория `overmindv/`; стартовые команды для всего инфраструктурного окружения находятся в корневом `CLAUDE.md` (вложенная директория `overmindv/media/..` описана в `overmindv/CLAUDE.md`).

## Commands (Makefile)

```bash
make run          # запуск сервиса (API + фоновый worker в одном процессе)
make build        # сборка бинарника
make test         # go test -race ./...
make lint         # golangci-lint (v2.1.6, ставится в ./bin)
make migrate-up   # накатить миграции (goose, --dir migrations)
make migrate-down # откатить последнюю миграцию
make migrate-status
make tidy
```

- Один тест: `go test -race ./internal/<pkg>/ -run <TestName>` (например `go test -race ./internal/domain/ -run TestMedia`).
- DSN миграций по умолчанию: `postgres://media:media@localhost:5435/media`. Основной локальный запуск всей среды — `make up` из `../infra`; для изолированной разработки: `cp .env.example .env && make migrate-up && make run`.
- Конфигурация — переменные окружения `MEDIA_*` с дефолтами в `internal/config/config.go` (S3 endpoint по умолчанию `localhost:9000` — MinIO, path-style).

## Architecture

Go 1.26, модуль `github.com/overmindv/media`. Каркас — `parker` (`cmd/media/main.go` → `parker.Main(run, parker.WithAppName("media"))`); parker владеет пулом PostgreSQL, HTTP-сервером, `/health`, `/ready`, `/metrics` и логгированием. DI-сборка зависимостей — в `internal/app/media/container.go` (`Build`).

Слои (сверху вниз):

- `internal/httpapi` — внутренний REST API. Роуты регистрируются в `Register()`; все endpoint-ы требуют `X-Media-Service-Token` (отдельные токены gateway и users). Контекст пользователя приходит от gateway в `X-User-ID` / `X-User-Roles`. Auth + проверку ownership на каждый файл делает `internalAuth` / `gatewayOnly`.
- `internal/service` — бизнес-логика. `service.contracts.go` объявляет интерфейсы `Repository` и `ObjectStorage` (тесты реализуют их двойниками); `Service` держит инъектируемый `now func() time.Time`. Возвращает доменные объекты из `internal/domain`.
- `internal/domain` — сущности: `File`, `UploadSession`, `Actor`, `Blob`, `Variant`, enum-ы `Purpose`/`Visibility`/`Status`/`UploadMode` + помощники прав доступа `File.CanRead`/`CanDelete`, `Actor.IsAdmin`.
- `internal/repository` — PostgreSQL (`postgres.go`) и доступ к worker-очереди (`worker.go`), pgx/v5.
- `internal/storage` — S3-адаптер (`s3.go`), AWS SDK v2 (presigned POST/multipart, Copy/Head/Get/Put/Delete).
- `internal/worker` — фоновый обработчик (`processor.go`, `clamav.go`), зарегистрирован как Runnable `media-worker`. Опрашивает очередь PostgreSQL, обрабатывает задания `scan_and_process` и `purge`; в отдельном worker-бинарнике не нуждается.
- `internal/apperror` — коды ошибок и их маппинг в HTTP-статусы.

### Поток загрузки (байты не проходят через gateway/API/PostgreSQL)

1. Frontend → GraphQL `createMediaUpload` в api-gateway → доверенный actor context → внутренний HTTP API Media.
2. Media создаёт `pending_upload` и возвращает presigned POST либо multipart target.
3. Браузер загружает байты напрямую в MinIO/S3, затем зовёт `completeMediaUpload`.
4. Worker (`media-jobs` из PostgreSQL): сверяет checksum, сканирует ClamAV (degrading — если clamd недоступен, сканирование пропускается), определяет фактический MIME через `http.DetectContentType` + `image.DecodeConfig`, генерирует WebP-варианты через CLI `vipsthumbnail` (libvips) и переводит файл в `ready`.

Поддерживаемые типы и лимиты зависят от `Purpose` (avatar 5 MiB/12 Мп, image 20 MiB/40 Мп, документы 50 MiB, архивы 250 MiB; изображения получают варианты 320/768/1440, аватары — 128/320/768). EXIF удаляется. WebP-вариант без upscale не создаётся, если исходник уже не шире целевой ширины.

### Схема БД (goose, `migrations/`)

`blobs` (дедупликация по частичному уникальному индексу `checksum_sha256 + size_bytes + visibility` для активных clean-записей), `files`, `blob_variants`, `upload_sessions` (1:1 с file, `ON DELETE CASCADE`), `file_bindings` (связи с внешними сервисами/entities), `media_jobs` (очередь, опрашивается worker-ом через `ClaimJob`/`RetryJob`), `outbox_events`. Асинхронность — пул в PostgreSQL + polling, без Kafka/Redis.

## Советы

- Антивирус опционален: решение «работать без clamd» принимается в `container.go` по `Available(3s)`.
- При добавлении endpoint-а не забывайте про выбор токен-контракта (gateway-only vs users-only) и проверку доступа к файлу на уровне `domain.File.Can*`.
