# Интеграция Media с Entities

Репозиторий Mirage пока не содержит приложения. Ironhide хранит nullable `logo_file_id` университета как внешний UUID без межсервисного foreign key.

Mirage должен предоставить загрузку файлов и выдачу URL через API Gateway. Frontend сначала загружает логотип через GraphQL API Gateway, затем передаёт полученный file ID в `createUniversity` или `updateUniversity`. Ironhide не загружает, не удаляет и не раздаёт файлы.
