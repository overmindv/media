package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

const cleanupBatch = 20

func (r *Postgres) ScheduleUploadCleanup(ctx context.Context, cutoff time.Time) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
            SELECT s.file_id FROM upload_sessions s JOIN files f ON f.id=s.file_id
             WHERE s.cleanup_queued_at IS NULL AND s.expires_at<=$1 AND (
               f.status IN ('pending_upload','ready','rejected','deleted')
               OR (f.status IN ('quarantined','processing') AND EXISTS(
                 SELECT 1 FROM media_jobs j WHERE j.file_id=f.id AND j.kind='scan_and_process' AND j.status='failed'
               ))
             )
             AND NOT EXISTS(SELECT 1 FROM media_jobs j WHERE j.file_id=f.id AND j.kind='scan_and_process'
                              AND j.status IN ('pending','running'))
             AND (f.status='ready' OR NOT EXISTS(
               SELECT 1 FROM file_bindings b WHERE b.file_id=f.id AND b.deleted_at IS NULL
             ))
             ORDER BY s.expires_at,s.file_id LIMIT $2 FOR UPDATE OF f SKIP LOCKED`, cutoff, cleanupBatch)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return err
		}
		if lockErr := lockQuotaBatch(ctx, tx, ids); lockErr != nil {
			return lockErr
		}
		_, err = tx.Exec(ctx, `
            WITH expired AS (
                UPDATE upload_sessions s SET cleanup_queued_at=statement_timestamp()
                 FROM files f WHERE s.file_id=f.id AND f.id=ANY($1::uuid[]) AND s.cleanup_queued_at IS NULL
                   AND NOT EXISTS(SELECT 1 FROM media_jobs j WHERE j.file_id=f.id AND j.kind='scan_and_process'
                                   AND j.status IN ('pending','running'))
                   AND (
                     f.status='ready' OR NOT EXISTS(
                       SELECT 1 FROM file_bindings b WHERE b.file_id=f.id AND b.deleted_at IS NULL
                     )
                   )
                 RETURNING s.file_id
            ), rejected AS (
                UPDATE files f SET status='rejected',failure_code=CASE
                    WHEN f.status='pending_upload' THEN $2 ELSE $3 END, blob_id=NULL,updated_at=NOW()
                  FROM expired e WHERE f.id=e.file_id AND f.status IN ('pending_upload','quarantined','processing')
                RETURNING f.id,f.failure_code
            ), detached AS (
                UPDATE files f SET blob_id=NULL,updated_at=NOW()
                  FROM expired e WHERE f.id=e.file_id AND f.status='rejected' AND f.blob_id IS NOT NULL
            ), events AS (
                INSERT INTO outbox_events(id,aggregate_type,aggregate_id,event_type,payload)
                SELECT gen_random_uuid(),'file',id,'file.rejected',
                       jsonb_build_object('file_id',id::text,'code',failure_code) FROM rejected
            )
            INSERT INTO media_jobs(id,file_id,kind)
            SELECT gen_random_uuid(),file_id,'cleanup_upload' FROM expired`, ids, apperror.UploadExpired, apperror.ProcessingFailed)

		return err
	})
}

func (r *Postgres) PrepareUploadCleanup(ctx context.Context, job domain.Job) (domain.UploadSession, error) {
	var session domain.UploadSession
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := lockJob(ctx, tx, job); err != nil {
			return err
		}
		var err error
		session, err = scanUploadSession(tx.QueryRow(ctx, `
            SELECT s.file_id,s.mode,s.bucket,s.object_key,s.multipart_id,s.expected_size,
                   s.checksum_sha256,s.content_type,s.expires_at,s.completed_at,s.part_size
              FROM upload_sessions s JOIN files f ON f.id=s.file_id
             WHERE s.file_id=$1 AND s.cleanup_queued_at IS NOT NULL AND s.cleaned_at IS NULL
               AND f.status IN ('ready','rejected','deleted')`, job.FileID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		return err
	})

	return session, err
}

func (r *Postgres) CompleteUploadCleanup(ctx context.Context, job domain.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := lockJob(ctx, tx, job); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE upload_sessions SET cleaned_at=COALESCE(cleaned_at,statement_timestamp())
            WHERE file_id=$1 AND cleanup_queued_at IS NOT NULL`, job.FileID); err != nil {
			return err
		}

		return finishJob(ctx, tx, job)
	})
}

const stagedCandidates = `
    SELECT o.object_key FROM media_job_objects o JOIN media_jobs j ON j.id=o.job_id
     WHERE o.created_at<=$1 AND j.updated_at<=$1 AND (
       j.status<>'running' OR j.lease_token<>o.lease_token OR j.lease_until<=$1
     )
     AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.object_key=o.object_key)
     AND NOT EXISTS (SELECT 1 FROM blob_variants v WHERE v.object_key=o.object_key)`

func (r *Postgres) StagedCleanupTargets(ctx context.Context, cutoff time.Time) ([]string, error) {
	var targets []string
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, stagedCandidates+`
            ORDER BY o.created_at,o.object_key LIMIT $2 FOR UPDATE OF j SKIP LOCKED`, cutoff, cleanupBatch)
		if err != nil {
			return err
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(keys) == 0 {
			return err
		}
		// Recheck the journal after the job lock; a commit may have adopted a staged key.
		rows, err = tx.Query(ctx, stagedCandidates+`
            AND o.object_key=ANY($2::text[])`, cutoff, keys)
		if err != nil {
			return err
		}
		targets, err = pgx.CollectRows(rows, pgx.RowTo[string])

		return err
	})

	return targets, err
}

func (r *Postgres) CompleteStagedCleanup(ctx context.Context, key string) error {
	_, err := r.pool.Exec(ctx, "DELETE FROM media_job_objects WHERE object_key=$1", key)

	return err
}

type BlobCleanup struct {
	ID   string
	Keys []string
}

func (r *Postgres) UnboundCleanupTargets(ctx context.Context, cutoff time.Time) ([]BlobCleanup, error) {
	var blobs []BlobCleanup
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
            SELECT b.id FROM blobs b WHERE b.cleaned_at IS NULL AND b.created_at<=$1
              AND NOT EXISTS(SELECT 1 FROM files f WHERE f.blob_id=b.id)
             ORDER BY b.created_at,b.id LIMIT $2 FOR UPDATE OF b SKIP LOCKED`, cutoff, cleanupBatch)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return err
		}
		rows, err = tx.Query(ctx, `
            WITH retired AS (
                UPDATE blobs b SET deleted_at=COALESCE(deleted_at,statement_timestamp())
                 WHERE b.id=ANY($1::uuid[]) AND NOT EXISTS(SELECT 1 FROM files f WHERE f.blob_id=b.id)
                 RETURNING b.id,b.object_key
            )
            SELECT b.id, ARRAY(SELECT b.object_key UNION ALL
                SELECT v.object_key FROM blob_variants v WHERE v.blob_id=b.id)
              FROM retired b`, ids)
		if err != nil {
			return err
		}
		blobs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[BlobCleanup])

		return err
	})

	return blobs, err
}

func (r *Postgres) CompleteUnboundCleanup(ctx context.Context, blobID string) error {
	_, err := r.pool.Exec(ctx, "UPDATE blobs SET cleaned_at=statement_timestamp() WHERE id=$1 AND deleted_at IS NOT NULL", blobID)

	return err
}
