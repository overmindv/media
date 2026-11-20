-- +goose Up
ALTER TABLE media_jobs ADD COLUMN lease_token UUID, ADD COLUMN lease_until TIMESTAMPTZ;
UPDATE media_jobs SET lease_until=statement_timestamp() WHERE status='running';
CREATE INDEX idx_media_jobs_expired ON media_jobs (lease_until,id) WHERE status='running';
CREATE INDEX idx_files_blob_id ON files (blob_id) WHERE blob_id IS NOT NULL;

CREATE TABLE media_job_objects (
    object_key TEXT PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES media_jobs(id),
    lease_token UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_media_job_objects_created ON media_job_objects (created_at,object_key);

-- +goose Down
DROP TABLE media_job_objects;
DROP INDEX idx_files_blob_id;
DROP INDEX idx_media_jobs_expired;
ALTER TABLE media_jobs DROP COLUMN lease_token, DROP COLUMN lease_until;
