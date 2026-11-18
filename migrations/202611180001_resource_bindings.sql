-- +goose Up
DROP INDEX uq_file_bindings_target_active;
CREATE UNIQUE INDEX uq_file_bindings_target_single
    ON file_bindings (service_name, entity_type, entity_id)
    WHERE deleted_at IS NULL AND service_name <> 'content';
CREATE INDEX idx_active_file_bindings_target
    ON file_bindings (service_name, entity_type, entity_id, file_id) WHERE deleted_at IS NULL;

CREATE TABLE resource_binding_versions (
    scope TEXT NOT NULL CHECK (scope IN ('content_revision', 'catalog_logo')),
    entity_id UUID NOT NULL,
    resource_id UUID NOT NULL,
    version BIGINT NOT NULL CHECK (version >= 0),
    request_hash BYTEA CHECK (octet_length(request_hash) = 32),
    PRIMARY KEY (scope, entity_id),
    CHECK ((version = 0) = (request_hash IS NULL)),
    CHECK (scope <> 'catalog_logo' OR entity_id = resource_id)
);

-- +goose Down
ALTER TABLE resource_binding_versions ADD CONSTRAINT no_versioned_bindings CHECK (version = 0);
DROP TABLE resource_binding_versions;
DROP INDEX idx_active_file_bindings_target;
DROP INDEX uq_file_bindings_target_single;
CREATE UNIQUE INDEX uq_file_bindings_target_active
    ON file_bindings (service_name, entity_type, entity_id)
    WHERE deleted_at IS NULL;
