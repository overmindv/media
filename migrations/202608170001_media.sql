-- +goose Up
CREATE TABLE blobs (
    id UUID PRIMARY KEY,
    checksum_sha256 TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    visibility TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
    detected_content_type TEXT NOT NULL,
    object_key TEXT NOT NULL UNIQUE,
    scan_status TEXT NOT NULL CHECK (scan_status IN ('clean', 'infected', 'failed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX uq_blobs_dedup_active
    ON blobs (checksum_sha256, size_bytes, visibility)
    WHERE deleted_at IS NULL AND scan_status = 'clean';

CREATE TABLE files (
    id UUID PRIMARY KEY,
    blob_id UUID REFERENCES blobs (id),
    owner_user_id UUID NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('avatar', 'catalog_logo', 'content_image', 'attachment', 'archive')),
    visibility TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
    original_name TEXT NOT NULL,
    declared_content_type TEXT NOT NULL,
    detected_content_type TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    checksum_sha256 TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending_upload', 'quarantined', 'processing', 'ready', 'rejected', 'deleted')),
    failure_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    purge_after TIMESTAMPTZ
);

CREATE INDEX idx_files_owner_created ON files (owner_user_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_files_status ON files (status, updated_at);

CREATE TABLE blob_variants (
    id UUID PRIMARY KEY,
    blob_id UUID NOT NULL REFERENCES blobs (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    format TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    size_bytes BIGINT NOT NULL,
    checksum_sha256 TEXT NOT NULL,
    object_key TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (blob_id, name)
);

CREATE TABLE upload_sessions (
    file_id UUID PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK (mode IN ('single', 'multipart')),
    bucket TEXT NOT NULL,
    object_key TEXT NOT NULL UNIQUE,
    multipart_id TEXT NOT NULL DEFAULT '',
    expected_size BIGINT NOT NULL,
    checksum_sha256 TEXT NOT NULL,
    content_type TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE file_bindings (
    id UUID PRIMARY KEY,
    file_id UUID NOT NULL REFERENCES files (id),
    service_name TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id UUID NOT NULL,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX uq_file_bindings_active
    ON file_bindings (file_id, service_name, entity_type, entity_id)
    WHERE deleted_at IS NULL;

CREATE TABLE media_jobs (
    id UUID PRIMARY KEY,
    file_id UUID NOT NULL REFERENCES files (id),
    kind TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_media_jobs_claim ON media_jobs (available_at, created_at) WHERE status = 'pending';

CREATE TABLE outbox_events (
    id UUID PRIMARY KEY,
    aggregate_type TEXT NOT NULL,
    aggregate_id UUID NOT NULL,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at TIMESTAMPTZ
);

CREATE INDEX idx_media_outbox_unpublished ON outbox_events (created_at) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS media_jobs;
DROP TABLE IF EXISTS file_bindings;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS blob_variants;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS blobs;
