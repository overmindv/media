-- +goose Up
CREATE UNIQUE INDEX uq_file_bindings_target_active
    ON file_bindings (service_name, entity_type, entity_id)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_files_orphan_avatars
    ON files (updated_at)
    WHERE purpose = 'avatar' AND status = 'ready' AND deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_files_orphan_avatars;
DROP INDEX IF EXISTS uq_file_bindings_target_active;
