# Версии аватаров

OVM-271 расширяет журнал resource_binding_versions областью user_avatar.
`PUT /v1/internal/users/{id}/avatar-binding/versioned` принимает только Users token
и JSON `{ "file_id": "UUID либо null", "version": 1 }`. Actor выводится из path,
чужой владелец, неготовый или непросканированный файл отклоняется.

Версия положительная; одинаковый запрос идемпотентен, старая версия игнорируется,
равная с другим файлом возвращает 409. Null сохраняет tombstone версии. Legacy
endpoint использует ту же блокировку и запрещает запись после versioned запроса.
Сначала применить миграцию 202611220001 и обновить Media, затем Users workers.
Изменения inventory/reconciliation OVM-180 в этот срез не входят.
