-- +goose Up
ALTER TABLE files ADD COLUMN upload_expires_at TIMESTAMPTZ, ADD COLUMN upload_multipart BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE upload_sessions ADD COLUMN part_size BIGINT NOT NULL DEFAULT 0 CHECK(part_size>=0);

UPDATE files f SET upload_expires_at=s.expires_at,upload_multipart=(s.mode='multipart')
  FROM upload_sessions s WHERE s.file_id=f.id AND f.status='pending_upload';

WITH rejected AS (
    UPDATE files f SET status='rejected',failure_code='PROCESSING_FAILED',updated_at=NOW()
     WHERE f.status IN ('quarantined','processing') AND f.deleted_at IS NULL
       AND EXISTS(SELECT 1 FROM media_jobs j WHERE j.file_id=f.id AND j.kind='scan_and_process' AND j.status='failed')
       AND NOT EXISTS(SELECT 1 FROM media_jobs j WHERE j.file_id=f.id AND j.kind='scan_and_process' AND j.status IN ('pending','running'))
     RETURNING f.id
)
INSERT INTO outbox_events(id,aggregate_type,aggregate_id,event_type,payload)
SELECT gen_random_uuid(),'file',id,'file.rejected',jsonb_build_object('file_id',id::text,'code','PROCESSING_FAILED') FROM rejected;

CREATE INDEX idx_files_quota_expired ON files (owner_user_id,upload_expires_at,id)
    WHERE status='pending_upload' AND deleted_at IS NULL;

CREATE TABLE media_user_quota (
    owner_user_id UUID PRIMARY KEY,
    used_bytes BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes>=0),
    multipart_sessions INTEGER NOT NULL DEFAULT 0 CHECK (multipart_sessions>=0)
);

CREATE TABLE media_quota_blob_refs (
    owner_user_id UUID NOT NULL,
    blob_id UUID NOT NULL REFERENCES blobs(id),
    ref_count BIGINT NOT NULL CHECK (ref_count>0),
    PRIMARY KEY (owner_user_id,blob_id)
);

INSERT INTO media_quota_blob_refs(owner_user_id,blob_id,ref_count)
SELECT owner_user_id,blob_id,count(*) FROM files
 WHERE status='ready' AND deleted_at IS NULL AND blob_id IS NOT NULL
 GROUP BY owner_user_id,blob_id;

INSERT INTO media_user_quota(owner_user_id,used_bytes,multipart_sessions)
SELECT owner_user_id,sum(bytes),sum(multipart_sessions)
  FROM (
    SELECT owner_user_id,
           sum(CASE WHEN status='ready' AND blob_id IS NOT NULL THEN 0 ELSE size_bytes END) AS bytes,
           count(*) FILTER(WHERE status='pending_upload' AND upload_multipart) AS multipart_sessions
      FROM files WHERE status NOT IN ('deleted','rejected') AND deleted_at IS NULL GROUP BY owner_user_id
    UNION ALL
    SELECT r.owner_user_id,sum(b.size_bytes),0 FROM media_quota_blob_refs r JOIN blobs b ON b.id=r.blob_id
     GROUP BY r.owner_user_id
  ) usage GROUP BY owner_user_id;

-- +goose StatementBegin
CREATE FUNCTION sync_media_quota() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    owner_id UUID;
    prior_blob UUID;
    next_blob UUID;
    bytes_delta BIGINT := 0;
    multipart_delta INTEGER := 0;
    next_refs BIGINT;
BEGIN
    IF TG_OP<>'INSERT' THEN
        owner_id := OLD.owner_user_id;
        IF OLD.deleted_at IS NULL AND OLD.status NOT IN ('deleted','rejected') THEN
            IF OLD.status='ready' AND OLD.blob_id IS NOT NULL THEN
                prior_blob := OLD.blob_id;
            ELSE
                bytes_delta := -OLD.size_bytes;
            END IF;
            IF OLD.status='pending_upload' AND OLD.upload_multipart THEN
                multipart_delta := -1;
            END IF;
        END IF;
    END IF;
    IF TG_OP<>'DELETE' THEN
        IF TG_OP='UPDATE' AND OLD.owner_user_id<>NEW.owner_user_id THEN
            RAISE EXCEPTION 'file owner cannot change';
        END IF;
        owner_id := NEW.owner_user_id;
        IF NEW.deleted_at IS NULL AND NEW.status NOT IN ('deleted','rejected') THEN
            IF NEW.status='ready' AND NEW.blob_id IS NOT NULL THEN
                next_blob := NEW.blob_id;
            ELSE
                bytes_delta := bytes_delta+NEW.size_bytes;
            END IF;
            IF NEW.status='pending_upload' AND NEW.upload_multipart THEN
                multipart_delta := multipart_delta+1;
            END IF;
        END IF;
    END IF;

    IF prior_blob IS NOT DISTINCT FROM next_blob AND bytes_delta=0 AND multipart_delta=0 THEN
        RETURN NULL;
    END IF;

    INSERT INTO media_user_quota(owner_user_id) VALUES(owner_id) ON CONFLICT DO NOTHING;
    PERFORM 1 FROM media_user_quota WHERE owner_user_id=owner_id FOR UPDATE;

    IF prior_blob IS DISTINCT FROM next_blob THEN
        IF prior_blob IS NOT NULL THEN
            DELETE FROM media_quota_blob_refs WHERE owner_user_id=owner_id AND blob_id=prior_blob AND ref_count=1;
            IF FOUND THEN
                bytes_delta := bytes_delta-(SELECT size_bytes FROM blobs WHERE id=prior_blob);
            ELSE
                UPDATE media_quota_blob_refs SET ref_count=ref_count-1
                 WHERE owner_user_id=owner_id AND blob_id=prior_blob;
            END IF;
        END IF;
        IF next_blob IS NOT NULL THEN
            INSERT INTO media_quota_blob_refs(owner_user_id,blob_id,ref_count) VALUES(owner_id,next_blob,1)
            ON CONFLICT(owner_user_id,blob_id) DO UPDATE SET ref_count=media_quota_blob_refs.ref_count+1
            RETURNING ref_count INTO next_refs;
            IF next_refs=1 THEN
                bytes_delta := bytes_delta+(SELECT size_bytes FROM blobs WHERE id=next_blob);
            END IF;
        END IF;
    END IF;

    UPDATE media_user_quota SET used_bytes=used_bytes+bytes_delta,multipart_sessions=multipart_sessions+multipart_delta
     WHERE owner_user_id=owner_id;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER files_quota AFTER INSERT OR DELETE OR UPDATE OF owner_user_id,status,blob_id,size_bytes,deleted_at,upload_multipart
    ON files FOR EACH ROW EXECUTE FUNCTION sync_media_quota();

-- +goose Down
DROP TRIGGER files_quota ON files;
DROP FUNCTION sync_media_quota();
DROP TABLE media_quota_blob_refs;
DROP TABLE media_user_quota;
DROP INDEX idx_files_quota_expired;
ALTER TABLE upload_sessions DROP COLUMN part_size;
ALTER TABLE files DROP COLUMN upload_multipart,DROP COLUMN upload_expires_at;
