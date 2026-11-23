# Интеграция Media с Entities

Логотип загружается через API Gateway с назначением `catalog_logo`.
Entities хранит UUID файла, а Media хранит привязку и защищает файл от
удаления, пока эта привязка действует.

Endpoints принимают только отдельный `MEDIA_ENTITIES_TOKEN` в заголовке
`X-Media-Service-Token`:

- `POST /v1/internal/entities/binding-files/check` проверяет готовность,
  видимость и владельца файла.
- `PUT /v1/internal/entities/logos/{entity_id}/bindings` заменяет привязку
  в новой версии. Пустой список снимает привязку, старая версия ее не возвращает.
- `GET /v1/internal/entities/logos/{entity_id}/bindings` возвращает версию
  и фактический `file_id`. Если записи о привязке нет, `file_id` равен `null`.
  Если нет истории версии, `version` равен `0`.

Media не читает таблицы Entities. Сквозной тест запускается из Infra
командой `sh scripts/test-logo-bindings.sh` на временных базах.
