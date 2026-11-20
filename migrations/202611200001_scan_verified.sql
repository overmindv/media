-- +goose Up
ALTER TABLE blobs ADD COLUMN scan_verified BOOLEAN NOT NULL DEFAULT FALSE;

DROP INDEX uq_blobs_dedup_active;
CREATE UNIQUE INDEX uq_blobs_dedup_active
    ON blobs (checksum_sha256, size_bytes, visibility, scan_verified)
    WHERE deleted_at IS NULL AND scan_status = 'clean';

CREATE INDEX idx_media_jobs_scan_backlog ON media_jobs (id)
    WHERE kind = 'scan_and_process' AND status IN ('pending', 'running');

-- +goose Down
DROP INDEX idx_media_jobs_scan_backlog;
DROP INDEX uq_blobs_dedup_active;
CREATE UNIQUE INDEX uq_blobs_dedup_active
    ON blobs (checksum_sha256, size_bytes, visibility)
    WHERE deleted_at IS NULL AND scan_status = 'clean';
ALTER TABLE blobs DROP COLUMN scan_verified;
