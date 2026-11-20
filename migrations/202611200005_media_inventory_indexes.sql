-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_blobs_inventory_key ON blobs (object_key COLLATE "C");
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_variants_inventory_key ON blob_variants (object_key COLLATE "C");
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_staged_inventory_key ON media_job_objects (object_key COLLATE "C");
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_uploads_inventory_key ON upload_sessions (bucket, object_key COLLATE "C");

-- +goose StatementBegin
DO $$
DECLARE invalid_name text;
BEGIN
    SELECT c.relname INTO invalid_name
      FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
      JOIN pg_namespace n ON n.oid=c.relnamespace
     WHERE n.nspname=current_schema() AND NOT i.indisvalid
       AND c.relname IN ('idx_blobs_inventory_key', 'idx_variants_inventory_key',
                         'idx_staged_inventory_key', 'idx_uploads_inventory_key')
     LIMIT 1;
    IF invalid_name IS NOT NULL THEN
        RAISE EXCEPTION 'Invalid inventory index %; rebuild it before retrying', invalid_name;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_uploads_inventory_key;
DROP INDEX CONCURRENTLY IF EXISTS idx_staged_inventory_key;
DROP INDEX CONCURRENTLY IF EXISTS idx_variants_inventory_key;
DROP INDEX CONCURRENTLY IF EXISTS idx_blobs_inventory_key;
