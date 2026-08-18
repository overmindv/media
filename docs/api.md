# Внутренний API Media

Все ответы об ошибках имеют форму `{ "code": "SNAKE_CASE", "message": "..." }`.

## Endpoints

- `POST /v1/uploads` — создать single или multipart upload;
- `POST /v1/uploads/{id}/parts` — подписать номера multipart-частей;
- `POST /v1/uploads/{id}/complete` — завершить загрузку и поставить scan job;
- `GET /v1/files/{id}` — получить метаданные с проверкой доступа;
- `GET /v1/files?limit=20&offset=0` — получить файлы текущего пользователя;
- `POST /v1/files/{id}/download-url` — получить публичную или временную приватную ссылку;
- `DELETE /v1/files/{id}` — выполнить soft delete;
- `POST /v1/internal/public-files/resolve` — одним запросом разрешить до 100 готовых публичных файлов и URL вариантов, доступно gateway;
- `GET /v1/internal/users/{user_id}/avatar-files/{file_id}/validate` — проверить ownership и готовность аватара, доступно только Users;
- `PUT /v1/internal/users/{user_id}/avatar-binding` — идемпотентно заменить или очистить avatar binding, доступно только Users;
- `GET /health`, `GET /ready`, `GET /metrics` — probes и Prometheus metrics.

Пример создания upload:

```json
{
  "original_name": "avatar.jpg",
  "content_type": "image/jpeg",
  "size_bytes": 102400,
  "checksum_sha256": "64 hex characters",
  "purpose": "avatar",
  "visibility": "public"
}
```

`CompleteUpload` идемпотентен: повтор после перехода из `pending_upload` возвращает текущее состояние файла и не создаёт второй job.

Для `download-url` допустимы варианты `original`, `w128`, `w320`, `w768` и `w1440`. Вариант без upscale не создаётся, если исходное изображение уже не шире соответствующего размера.
