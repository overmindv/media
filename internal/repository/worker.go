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
        WITH candidates AS (
            SELECT f.id
              FROM files f
             WHERE f.purpose='avatar' AND f.status='ready' AND f.deleted_at IS NULL
               AND f.updated_at < NOW() - $1::interval
               AND NOT EXISTS (
                    SELECT 1 FROM file_bindings b WHERE b.file_id=f.id AND b.deleted_at IS NULL
               )
             ORDER BY f.updated_at
             FOR UPDATE SKIP LOCKED LIMIT 20
        )
        UPDATE files f
           SET status='deleted',deleted_at=NOW(),purge_after=NOW()+$2::interval,updated_at=NOW()
          FROM candidates c WHERE f.id=c.id
        RETURNING f.id`, orphanAge.String(), retention.String())
	if err != nil {
		return fmt.Errorf("выбрать orphan avatars: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("прочитать orphan avatar: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("обойти orphan avatars: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		_, err = tx.Exec(ctx, `
            INSERT INTO media_jobs (id,file_id,kind,available_at) VALUES ($1,$2,'purge',NOW()+$3::interval)`, uuid.NewString(), id, retention.String())
		if err != nil {
			return fmt.Errorf("создать orphan purge job: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать orphan cleanup: %w", err)
	}

	return nil
}

// ClaimJob атомарно захватывает доступный job через SKIP LOCKED.
func (r *Postgres) ClaimJob(ctx context.Context) (domain.Job, bool, error) {
	var job domain.Job
	err := r.pool.QueryRow(ctx, `
        WITH candidate AS (
            SELECT id FROM media_jobs
             WHERE status='pending' AND available_at <= NOW()
             ORDER BY available_at, created_at
             FOR UPDATE SKIP LOCKED LIMIT 1
        )
        UPDATE media_jobs j
           SET status='running', locked_at=NOW(), attempts=attempts+1, updated_at=NOW()
          FROM candidate c WHERE j.id=c.id
        RETURNING j.id,j.file_id,j.kind,j.attempts`).Scan(&job.ID, &job.FileID, &job.Kind, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, fmt.Errorf("захватить media job: %w", err)
	}

	return job, true, nil
}

// MarkProcessing переводит quarantine-файл в processing перед тяжёлой обработкой.
func (r *Postgres) MarkProcessing(ctx context.Context, fileID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE files SET status='processing',updated_at=NOW() WHERE id=$1 AND status='quarantined'`, fileID)
	if err != nil {
		return fmt.Errorf("перевести файл в processing: %w", err)
	}

	return nil
}

// LinkCleanBlob создаёт или переиспользует чистый blob и связывает его с файлом.
func (r *Postgres) LinkCleanBlob(ctx context.Context, file domain.File, detectedType, objectKey string) (domain.Blob, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Blob{}, false, fmt.Errorf("начать blob transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	blob := domain.Blob{
		ID:                  uuid.NewString(),
		ChecksumSHA256:      file.ChecksumSHA256,
		SizeBytes:           file.SizeBytes,
		Visibility:          file.Visibility,
		DetectedContentType: detectedType,
		ObjectKey:           objectKey,
	}
	command, err := tx.Exec(ctx, `
        INSERT INTO blobs (id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key,scan_status)
        VALUES ($1,$2,$3,$4,$5,$6,'clean')
        ON CONFLICT (checksum_sha256,size_bytes,visibility)
        WHERE deleted_at IS NULL AND scan_status='clean' DO NOTHING`,
		blob.ID, blob.ChecksumSHA256, blob.SizeBytes, blob.Visibility, blob.DetectedContentType, blob.ObjectKey,
	)
	if err != nil {
		return domain.Blob{}, false, fmt.Errorf("вставить clean blob: %w", err)
	}
	deduplicated := command.RowsAffected() == 0
	if deduplicated {
		err = tx.QueryRow(ctx, `
            SELECT id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key
              FROM blobs
             WHERE checksum_sha256=$1 AND size_bytes=$2 AND visibility=$3
               AND deleted_at IS NULL AND scan_status='clean'`,
			file.ChecksumSHA256, file.SizeBytes, file.Visibility,
		).Scan(&blob.ID, &blob.ChecksumSHA256, &blob.SizeBytes, &blob.Visibility, &blob.DetectedContentType, &blob.ObjectKey)
		if err != nil {
			return domain.Blob{}, false, fmt.Errorf("получить deduplicated blob: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `UPDATE files SET blob_id=$2,detected_content_type=$3,updated_at=NOW() WHERE id=$1`, file.ID, blob.ID, detectedType)
	if err != nil {
		return domain.Blob{}, false, fmt.Errorf("связать файл с blob: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Blob{}, false, fmt.Errorf("зафиксировать blob transaction: %w", err)
	}

	return blob, deduplicated, nil
}

// SaveVariants атомарно сохраняет варианты, готовит файл и завершает job.
func (r *Postgres) SaveVariants(ctx context.Context, job domain.Job, fileID string, variants []domain.Variant) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать variants transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, variant := range variants {
		_, err = tx.Exec(ctx, `
            INSERT INTO blob_variants (id,blob_id,name,format,width,height,size_bytes,checksum_sha256,object_key,created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
            ON CONFLICT (blob_id,name) DO NOTHING`,
			variant.ID, variant.BlobID, variant.Name, variant.Format, variant.Width, variant.Height,
			variant.SizeBytes, variant.ChecksumSHA256, variant.ObjectKey, variant.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("сохранить variant %s: %w", variant.Name, err)
		}
	}
	_, err = tx.Exec(ctx, `UPDATE files SET status='ready',failure_code='',updated_at=NOW() WHERE id=$1 AND status='processing'`, fileID)
	if err != nil {
		return fmt.Errorf("пометить файл ready: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE media_jobs SET status='completed',updated_at=NOW() WHERE id=$1`, job.ID)
	if err != nil {
		return fmt.Errorf("завершить media job: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"file_id": fileID})
	if err != nil {
		return fmt.Errorf("сериализовать ready event: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO outbox_events (id,aggregate_type,aggregate_id,event_type,payload)
        VALUES ($1,'file',$2,'file.ready',$3)`, uuid.NewString(), fileID, payload)
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
	_, err = tx.Exec(ctx, `UPDATE files SET status='rejected',failure_code=$2,updated_at=NOW() WHERE id=$1`, job.FileID, failureCode)
	if err != nil {
		return fmt.Errorf("отклонить файл: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE media_jobs SET status='completed',updated_at=NOW() WHERE id=$1`, job.ID)
	if err != nil {
		return fmt.Errorf("завершить rejected job: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO outbox_events (id,aggregate_type,aggregate_id,event_type,payload)
        VALUES ($1,'file',$2,'file.rejected',jsonb_build_object('file_id',$2::text,'code',$3::text))`,
		uuid.NewString(), job.FileID, failureCode,
	)
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
	_, err := r.pool.Exec(ctx, `
        UPDATE media_jobs
           SET status=CASE WHEN attempts >= 10 THEN 'failed' ELSE 'pending' END,
               available_at=$2,last_error=$3,locked_at=NULL,updated_at=NOW()
         WHERE id=$1`, job.ID, time.Now().Add(delay), message)
	if err != nil {
		return fmt.Errorf("вернуть media job в очередь: %w", err)
	}

	return nil
}

// PurgeTargets возвращает физические ключи только когда blob больше не используется активными файлами.
func (r *Postgres) PurgeTargets(ctx context.Context, fileID string) ([]string, error) {
	var blobID *string
	if err := r.pool.QueryRow(ctx, `SELECT blob_id FROM files WHERE id=$1 AND deleted_at IS NOT NULL`, fileID).Scan(&blobID); err != nil {
		return nil, fmt.Errorf("получить blob удалённого файла: %w", err)
	}
	if blobID == nil {
		return nil, nil
	}
	var activeReferences int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM files WHERE blob_id=$1 AND deleted_at IS NULL`, *blobID).Scan(&activeReferences); err != nil {
		return nil, fmt.Errorf("посчитать активные blob references: %w", err)
	}
	if activeReferences > 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
        SELECT object_key FROM blobs WHERE id=$1
        UNION ALL
        SELECT object_key FROM blob_variants WHERE blob_id=$1`, *blobID)
	if err != nil {
		return nil, fmt.Errorf("получить purge targets: %w", err)
	}
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("прочитать purge target: %w", err)
		}
		keys = append(keys, key)
	}

	return keys, rows.Err()
}

// CompletePurge завершает GC job и помечает неиспользуемый blob удалённым.
func (r *Postgres) CompletePurge(ctx context.Context, job domain.Job) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать purge transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
        UPDATE blobs SET deleted_at=NOW(),updated_at=NOW()
         WHERE id=(SELECT blob_id FROM files WHERE id=$1)
           AND NOT EXISTS (SELECT 1 FROM files WHERE blob_id=blobs.id AND deleted_at IS NULL)`, job.FileID)
	if err != nil {
		return fmt.Errorf("пометить blob удалённым: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE media_jobs SET status='completed',updated_at=NOW() WHERE id=$1`, job.ID)
	if err != nil {
		return fmt.Errorf("завершить purge job: %w", err)
	}

	return tx.Commit(ctx)
}
