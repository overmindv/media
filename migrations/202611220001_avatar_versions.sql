-- +goose Up
ALTER TABLE resource_binding_versions DROP CONSTRAINT resource_binding_versions_scope_check;
ALTER TABLE resource_binding_versions ADD CONSTRAINT resource_binding_versions_scope_check
    CHECK (scope IN ('content_revision', 'catalog_logo', 'user_avatar'));

-- +goose Down
ALTER TABLE resource_binding_versions DROP CONSTRAINT resource_binding_versions_scope_check;
ALTER TABLE resource_binding_versions ADD CONSTRAINT resource_binding_versions_scope_check
    CHECK (scope IN ('content_revision', 'catalog_logo'));
