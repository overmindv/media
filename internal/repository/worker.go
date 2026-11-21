package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/domain"
)

// ScheduleOrphanAvatars переводит непривязанные готовые аватары в отложенный GC.
func (r *Postgres) ScheduleOrphanAvatars(ctx context.Context, orphanAge, retention time.Duration) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать orphan cleanup transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
        SELECT f.id FROM files f
        WHERE f.purpose='avatar' AND f.status='ready' AND f.deleted_at IS NULL
          AND f.updated_at < NOW() - $1::interval
          AND NOT EXISTS (SELECT 1 FROM file_bindings b WHERE b.file_id=f.id AND b.deleted_at IS NULL)
        ORDER BY f.updated_at
        FOR UPDATE SKIP LOCKED LIMIT 20`, orphanAge.String())
	if err != nil {
		return fmt.Errorf("выбрать orphan avatars: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return fmt.Errorf("прочитать orphan avatar: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("обойти orphan avatars: %w", rowsErr)
	}
	rows.Close()
	if len(ids) > 0 {
		// После блокировки файлов следующий запрос увидит недавно добавленные привязки.
		_, err = tx.Exec(ctx, `
            WITH removed AS (
                UPDATE files f
                SET status='deleted',deleted_at=NOW(),purge_after=NOW()+$2::interval,updated_at=NOW()
                WHERE f.id=ANY($1::uuid[])
                  AND NOT EXISTS (SELECT 1 FROM file_bindings b WHERE b.file_id=f.id AND b.deleted_at IS NULL)
                RETURNING f.id, f.purge_after
            )
            INSERT INTO media_jobs (id,file_id,kind,available_at)
            SELECT gen_random_uuid(), id, 'purge', purge_after FROM removed`, ids, retention.String())
		if err != nil {
			return fmt.Errorf("создать orphan purge job: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать orphan cleanup: %w", err)
	}

	return nil
}

func (r *Postgres) MarkProcessing(ctx context.Context, job domain.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := lockJob(ctx, tx, job); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE files SET status='processing',updated_at=NOW()
            WHERE id=$1 AND deleted_at IS NULL AND status IN ('quarantined','processing')`, job.FileID)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrScanCancelled
		}

		return err
	})
}

// LinkCleanBlob создаёт или переиспользует чистый blob и связывает его с файлом.
func (r *Postgres) LinkCleanBlob(ctx context.Context, job domain.Job, file domain.File, detectedType, objectKey string, scanVerified bool) (domain.Blob, bool, error) {
	if file.ID != job.FileID {
		return domain.Blob{}, false, ErrScanCancelled
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Blob{}, false, fmt.Errorf("начать blob transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if lockErr := lockJob(ctx, tx, job); lockErr != nil {
		return domain.Blob{}, false, lockErr
	}
	var locked int
	err = tx.QueryRow(ctx, `SELECT 1 FROM files
        WHERE id=$1 AND status='processing' AND deleted_at IS NULL FOR UPDATE`, job.FileID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Blob{}, false, ErrScanCancelled
	}
	if err != nil {
		return domain.Blob{}, false, err
	}
	blob := domain.Blob{
		ID:                  uuid.NewString(),
		ChecksumSHA256:      file.ChecksumSHA256,
		SizeBytes:           file.SizeBytes,
		Visibility:          file.Visibility,
		DetectedContentType: detectedType,
		ObjectKey:           objectKey,
	}
	command, err := tx.Exec(ctx, `
        INSERT INTO blobs (id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key,scan_status,scan_verified)
        VALUES ($1,$2,$3,$4,$5,$6,'clean',$7)
        ON CONFLICT (checksum_sha256,size_bytes,visibility,scan_verified)
        WHERE deleted_at IS NULL AND scan_status='clean' DO NOTHING`,
		blob.ID, blob.ChecksumSHA256, blob.SizeBytes, blob.Visibility, blob.DetectedContentType, blob.ObjectKey, scanVerified,
	)
	if err != nil {
		return domain.Blob{}, false, fmt.Errorf("вставить clean blob: %w", err)
	}
	deduplicated := command.RowsAffected() == 0
	if deduplicated {
		err = tx.QueryRow(ctx, `
            SELECT id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key
              FROM blobs
             WHERE checksum_sha256=$1 AND size_bytes=$2 AND visibility=$3 AND scan_verified=$4
               AND deleted_at IS NULL AND scan_status='clean' FOR UPDATE`,
			file.ChecksumSHA256, file.SizeBytes, file.Visibility, scanVerified,
		).Scan(&blob.ID, &blob.ChecksumSHA256, &blob.SizeBytes, &blob.Visibility, &blob.DetectedContentType, &blob.ObjectKey)
		if err != nil {
			return domain.Blob{}, false, fmt.Errorf("получить deduplicated blob: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE files SET blob_id=$2,detected_content_type=$3,updated_at=NOW()
        WHERE id=$1 AND deleted_at IS NULL AND status='processing'`, job.FileID, blob.ID, detectedType)
	if err != nil {
		return domain.Blob{}, false, fmt.Errorf("связать файл с blob: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return domain.Blob{}, false, ErrScanCancelled
	}
	if !deduplicated {
		if err := untrackObject(ctx, tx, job, objectKey); err != nil {
			return domain.Blob{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Blob{}, false, fmt.Errorf("зафиксировать blob transaction: %w", err)
	}

	return blob, deduplicated, nil
}

func (r *Postgres) VariantNames(ctx context.Context, blobID, prefix string) (map[string]bool, error) {
	rows, err := r.pool.Query(ctx, `SELECT name FROM blob_variants
        WHERE blob_id=$1 AND format='webp' AND starts_with(object_key,$2)`, blobID, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}

	return names, rows.Err()
}

// SaveVariants атомарно сохраняет варианты, готовит файл и завершает job.
func (r *Postgres) SaveVariants(ctx context.Context, job domain.Job, variantPrefix string, variants []domain.Variant) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать variants transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if lockErr := lockJob(ctx, tx, job); lockErr != nil {
		return lockErr
	}
	tag, err := tx.Exec(ctx, `
        UPDATE files SET status='ready',failure_code='',updated_at=NOW()
         WHERE id=$1 AND status='processing' AND deleted_at IS NULL`, job.FileID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrScanCancelled
	}
	for _, variant := range variants {
		saved, saveErr := tx.Exec(ctx, `
            INSERT INTO blob_variants (id,blob_id,name,format,width,height,size_bytes,checksum_sha256,object_key,created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
            ON CONFLICT (blob_id,name) DO UPDATE
                SET width=EXCLUDED.width,height=EXCLUDED.height,format=EXCLUDED.format,
                    size_bytes=EXCLUDED.size_bytes,checksum_sha256=EXCLUDED.checksum_sha256,
                    object_key=EXCLUDED.object_key
              WHERE NOT starts_with(blob_variants.object_key,$11)`,
			variant.ID, variant.BlobID, variant.Name, variant.Format, variant.Width, variant.Height,
			variant.SizeBytes, variant.ChecksumSHA256, variant.ObjectKey, variant.CreatedAt, variantPrefix,
		)
		if saveErr != nil {
			return fmt.Errorf("сохранить variant %s: %w", variant.Name, saveErr)
		}
		if saved.RowsAffected() == 1 {
			if untrackErr := untrackObject(ctx, tx, job, variant.ObjectKey); untrackErr != nil {
				return untrackErr
			}
		}
	}
	if finishErr := finishJob(ctx, tx, job); finishErr != nil {
		return finishErr
	}
	payload, err := json.Marshal(map[string]string{"file_id": job.FileID})
	if err != nil {
		return fmt.Errorf("сериализовать ready event: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO outbox_events (id,aggregate_type,aggregate_id,event_type,payload)
        VALUES ($1,'file',$2,'file.ready',$3)`, uuid.NewString(), job.FileID, payload)
	if err != nil {
		return fmt.Errorf("создать ready event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать variants transaction: %w", err)
	}

	return nil
}

// RejectFile фиксирует безопасный failure code и завершает job без публикации внутренних деталей.
func (r *Postgres) RejectFile(ctx context.Context, job domain.Job, failureCode string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать reject transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if lockErr := lockJob(ctx, tx, job); lockErr != nil {
		return lockErr
	}
	tag, err := tx.Exec(ctx, `UPDATE files SET status='rejected',failure_code=$2,updated_at=NOW()
        WHERE id=$1 AND deleted_at IS NULL AND status IN ('quarantined','processing')`, job.FileID, failureCode)
	if err != nil {
		return fmt.Errorf("отклонить файл: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrScanCancelled
	}
	if finishErr := finishJob(ctx, tx, job); finishErr != nil {
		return finishErr
	}
	payload, err := json.Marshal(map[string]string{"file_id": job.FileID, "code": failureCode})
	if err != nil {
		return fmt.Errorf("сериализовать rejected event: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO outbox_events (id,aggregate_type,aggregate_id,event_type,payload)
        VALUES ($1,'file',$2,'file.rejected',$3)`, uuid.NewString(), job.FileID, payload)
	if err != nil {
		return fmt.Errorf("создать rejected event: %w", err)
	}

	return tx.Commit(ctx)
}

// RetryJob возвращает временно упавший job в очередь с ограниченным exponential backoff.
func (r *Postgres) RetryJob(ctx context.Context, job domain.Job, processingErr error) error {
	message := processingErr.Error()
	if len(message) > 1000 {
		message = message[:1000]
	}
	delay := time.Duration(1<<min(job.Attempts, 8)) * time.Second
	tag, err := r.pool.Exec(ctx, `
        UPDATE media_jobs
           SET status=CASE WHEN attempts >= 10 AND kind<>'cleanup_upload' THEN 'failed' ELSE 'pending' END,
               available_at=$2,last_error=$3,locked_at=NULL,lease_token=NULL,lease_until=NULL,updated_at=NOW()
         WHERE id=$1 AND lease_token=$4 AND status='running' AND lease_until>statement_timestamp()`,
		job.ID, time.Now().Add(delay), message, job.LeaseToken)
	if err != nil {
		return fmt.Errorf("вернуть media job в очередь: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}

	return nil
}

func (r *Postgres) RetryScanJob(ctx context.Context, job domain.Job) error {
	delay := time.Duration(1<<min(job.Attempts, 8)) * time.Second
	var retried bool
	err := r.pool.QueryRow(ctx, `
        WITH retried AS (
            UPDATE media_jobs
               SET status='pending',available_at=$2,last_error='scan_unavailable',locked_at=NULL,
                   lease_token=NULL,lease_until=NULL,updated_at=NOW()
             WHERE id=$1 AND kind='scan_and_process' AND status='running'
               AND lease_token=$3 AND lease_until>statement_timestamp()
            RETURNING file_id
        ), reset AS (
            UPDATE files SET status='quarantined',updated_at=NOW()
             WHERE id IN (SELECT file_id FROM retried) AND status='processing'
        )
        SELECT EXISTS(SELECT 1 FROM retried)`, job.ID, time.Now().Add(delay), job.LeaseToken).Scan(&retried)
	if err != nil {
		return fmt.Errorf("reschedule scan job: %w", err)
	}
	if !retried {
		return ErrLeaseLost
	}

	return nil
}

func (r *Postgres) ScanBacklog(ctx context.Context) (int64, error) {
	var count int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM media_jobs
        WHERE kind='scan_and_process' AND status IN ('pending','running')`).Scan(&count)

	return count, err
}

// PreparePurge excludes the blob from dedup before returning any keys for S3 deletion.
func (r *Postgres) PreparePurge(ctx context.Context, job domain.Job) ([]string, error) {
	var keys []string
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := lockJob(ctx, tx, job); err != nil {
			return err
		}
		var blobID *string
		err := tx.QueryRow(ctx, `SELECT blob_id FROM files WHERE id=$1
            AND deleted_at IS NOT NULL AND purge_after<=statement_timestamp() FOR UPDATE`, job.FileID).Scan(&blobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || blobID == nil {
			return err
		}
		var locked int
		if lockErr := tx.QueryRow(ctx, `SELECT 1 FROM blobs WHERE id=$1 FOR UPDATE`, *blobID).Scan(&locked); lockErr != nil {
			return lockErr
		}
		tag, err := tx.Exec(ctx, `
            UPDATE blobs SET deleted_at=COALESCE(deleted_at,statement_timestamp()),updated_at=NOW()
             WHERE id=$1 AND NOT EXISTS (
               SELECT 1 FROM files f WHERE f.blob_id=$1 AND (
                 f.deleted_at IS NULL OR f.purge_after>statement_timestamp() OR EXISTS (
                   SELECT 1 FROM file_bindings b WHERE b.file_id=f.id AND b.deleted_at IS NULL
                 ) OR EXISTS (
                   SELECT 1 FROM upload_sessions s WHERE s.file_id=f.id AND s.completed_at IS NULL
                     AND s.expires_at>statement_timestamp()
                 )
               )
             )`, *blobID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT object_key FROM blobs WHERE id=$1
            UNION ALL SELECT object_key FROM blob_variants WHERE blob_id=$1`, *blobID)
		if err != nil {
			return err
		}
		keys, err = pgx.CollectRows(rows, pgx.RowTo[string])

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("prepare media purge: %w", err)
	}

	return keys, nil
}
