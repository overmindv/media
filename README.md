# media

`media` — владелец файлов и изображений Overmindv. PostgreSQL хранит только метаданные и жизненный цикл, бинарные данные находятся в S3-совместимом object storage.

## Поток загрузки

1. Frontend вызывает GraphQL `createMediaUpload` в `api-gateway`.
2. Gateway передаёт доверенный actor context во внутренний HTTP API Media.
3. Media создаёт `pending_upload` и возвращает presigned POST либо multipart upload.
4. Браузер загружает байты напрямую в MinIO/S3 и вызывает `completeMediaUpload`.
5. Worker проверяет checksum, ClamAV и фактический MIME, создаёт WebP-варианты и переводит файл в `ready`.

Байты не проходят через frontend API, gateway или PostgreSQL. Redis и ClickHouse сервису в базовой версии не нужны.

## Поддерживаемые файлы

- JPEG, PNG, WebP — до 20 MiB и 40 мегапикселей;
- avatar — только public JPEG/PNG/WebP до 5 MiB и 12 мегапикселей;
- PDF и OOXML — до 50 MiB;
- ZIP, TAR, GZIP — до 250 MiB;
- SVG, исполняемые файлы и видео отклоняются.

Обычные изображения получают WebP-варианты шириной 320, 768 и 1440 пикселей, аватары — 128, 320 и 768 пикселей. EXIF и другая metadata удаляются `libvips`.

Gateway разрешает публичные аватары batch-запросом до 100 `file_id`. Отдельный token Users даёт доступ только к идемпотентной замене binding `users/user_avatar/{user_id}`. Непривязанные готовые avatar-файлы старше 24 часов автоматически переводятся в soft delete.

## Запуск и проверка

Сервис запускается через каркас `parker`: `GET /health`, `GET /ready` (готовность включает PostgreSQL и S3), `GET /metrics` и middleware предоставляет parker. Ранее отдельный `media-worker` влит в основной бинарник как фоновой `Runnable` — обработка файлов (checksum, ClamAV, WebP-варианты) выполняется в том же процессе.

Основной локальный запуск выполняется из `../infra` командой `make up`. Для изолированной разработки:

```bash
cp .env.example .env
make migrate-up
make run
```

Проверки:

```bash
make test
make lint
make build
```

Внутренний API описан в [docs/api.md](docs/api.md). Все endpoints требуют `X-Media-Service-Token`. Gateway и Users используют разные tokens; пользовательский контекст принимается только от gateway в `X-User-ID` и `X-User-Roles`.
