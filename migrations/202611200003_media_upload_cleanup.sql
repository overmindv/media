-- +goose Up
ALTER TABLE upload_sessions ADD COLUMN cleanup_queued_at TIMESTAMPTZ, ADD COLUMN cleaned_at TIMESTAMPTZ;
ALTER TABLE blobs ADD COLUMN cleaned_at TIMESTAMPTZ;
CREATE INDEX idx_upload_sessions_cleanup ON upload_sessions (expires_at,file_id) WHERE cleanup_queued_at IS NULL;
CREATE INDEX idx_blobs_unbound_cleanup ON blobs (created_at,id) WHERE cleaned_at IS NULL;
CREATE INDEX idx_media_jobs_file_kind_status ON media_jobs (file_id,kind,status);

-- +goose Down
DROP INDEX idx_media_jobs_file_kind_status;
DROP INDEX idx_blobs_unbound_cleanup;
DROP INDEX idx_upload_sessions_cleanup;
ALTER TABLE blobs DROP COLUMN cleaned_at;
ALTER TABLE upload_sessions DROP COLUMN cleaned_at, DROP COLUMN cleanup_queued_at;
