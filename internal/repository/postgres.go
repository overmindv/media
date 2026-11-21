package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

type Postgres struct {
	pool                *pgxpool.Pool
	allowUnscannedLocal bool
}

// New создаёт PostgreSQL repository Media.
func New(pool *pgxpool.Pool, allowUnscannedLocal bool) *Postgres {
	return &Postgres{pool: pool, allowUnscannedLocal: allowUnscannedLocal}
}

// Ping проверяет соединение с PostgreSQL.
func (r *Postgres) Ping(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return nil
}

func (r *Postgres) ActiveSize(ctx context.Context, ownerUserID string) (int64, error) {
	var size int64
	if err := r.pool.QueryRow(ctx, `
        SELECT COALESCE((SELECT used_bytes FROM media_user_quota WHERE owner_user_id=$1),0)`, ownerUserID).Scan(&size); err != nil {
		return 0, fmt.Errorf("получить active media size: %w", err)
	}

	return size, nil
}

// CreateUpload атомарно сохраняет файл и upload-сессию.
func (r *Postgres) CreateUpload(ctx context.Context, file domain.File, session domain.UploadSession, quota domain.UploadQuota) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать транзакцию upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if expireErr := expireUploadIntents(ctx, tx, file.OwnerUserID); expireErr != nil {
		return expireErr
	}
	if _, initErr := tx.Exec(ctx, `INSERT INTO media_user_quota(owner_user_id) VALUES($1) ON CONFLICT DO NOTHING`, file.OwnerUserID); initErr != nil {
		return initErr
	}
	var used int64
	var multipart int
	if lockErr := tx.QueryRow(ctx, `SELECT used_bytes,multipart_sessions FROM media_user_quota
        WHERE owner_user_id=$1 FOR UPDATE`, file.OwnerUserID).Scan(&used, &multipart); lockErr != nil {
		return lockErr
	}
	var denied error
	if file.SizeBytes > quota.MaxBytes-used {
		denied = apperror.New(apperror.FileTooLarge, "превышена квота хранилища пользователя", http.StatusRequestEntityTooLarge)
	} else if session.Mode == domain.UploadModeMultipart && multipart >= quota.MaxMultipartSessions {
		denied = apperror.New(apperror.UploadLimitReached, "слишком много активных multipart-загрузок", http.StatusTooManyRequests)
	}
	if denied != nil {
		// Keep the expired reservations released even when this request cannot fit.
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return commitErr
		}

		return denied
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO files (
            id, owner_user_id, purpose, visibility, original_name, declared_content_type,
            size_bytes, checksum_sha256, status, created_at, updated_at, upload_expires_at, upload_multipart
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		file.ID, file.OwnerUserID, file.Purpose, file.Visibility, file.OriginalName,
		file.DeclaredContentType, file.SizeBytes, file.ChecksumSHA256, file.Status,
		file.CreatedAt, file.UpdatedAt, session.ExpiresAt, session.Mode == domain.UploadModeMultipart,
	)
	if err != nil {
		return fmt.Errorf("вставить файл: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO upload_sessions (
            file_id, mode, bucket, object_key, multipart_id, expected_size,
            checksum_sha256, content_type, expires_at, part_size
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		session.FileID, session.Mode, session.Bucket, session.ObjectKey, session.MultipartID,
		session.ExpectedSize, session.Checksum, session.ContentType, session.ExpiresAt, session.PartSize,
	)
	if err != nil {
		return fmt.Errorf("вставить upload-сессию: %w", err)
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return fmt.Errorf("зафиксировать upload-транзакцию: %w", commitErr)
	}

	return nil
}

func expireUploadIntents(ctx context.Context, tx pgx.Tx, ownerID string) error {
	_, err := tx.Exec(ctx, `
        WITH expired AS MATERIALIZED (
            SELECT id FROM files WHERE owner_user_id=$1 AND status='pending_upload' AND deleted_at IS NULL
              AND upload_expires_at<=statement_timestamp()
             ORDER BY upload_expires_at,id LIMIT 100 FOR UPDATE SKIP LOCKED
        ), released AS (
            UPDATE files f SET status='rejected',failure_code=$2,updated_at=NOW()
              FROM expired e WHERE f.id=e.id RETURNING f.id
        )
        INSERT INTO outbox_events(id,aggregate_type,aggregate_id,event_type,payload)
        SELECT gen_random_uuid(),'file',id,'file.rejected',jsonb_build_object('file_id',id::text,'code',$2::text)
          FROM released`, ownerID, apperror.UploadExpired)

	return err
}

func lockQuotaBatch(ctx context.Context, tx pgx.Tx, fileIDs []string) error {
	rows, err := tx.Query(ctx, `
        SELECT q.owner_user_id FROM media_user_quota q
         WHERE q.owner_user_id IN (SELECT owner_user_id FROM files WHERE id=ANY($1::uuid[]))
         ORDER BY q.owner_user_id FOR UPDATE`, fileIDs)
	if err != nil {
		return err
	}
	_, err = pgx.CollectRows(rows, pgx.RowTo[string])

	return err
}

func (r *Postgres) SetMultipartID(ctx context.Context, fileID, uploadID string) error {
	tag, err := r.pool.Exec(ctx, `
        WITH active AS (
            SELECT id FROM files WHERE id=$1 AND status='pending_upload' AND deleted_at IS NULL
              AND upload_expires_at>statement_timestamp() FOR UPDATE
        )
        UPDATE upload_sessions s SET multipart_id=$2 FROM active a
         WHERE s.file_id=a.id AND s.mode='multipart' AND s.multipart_id=''`, fileID, uploadID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return apperror.New(apperror.UploadExpired, "upload-сессия больше не активна", http.StatusGone)
	}

	return nil
}

func (r *Postgres) FailUploadIntent(ctx context.Context, fileID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE files SET status='rejected',failure_code=$2,updated_at=NOW()
        WHERE id=$1 AND status='pending_upload' AND deleted_at IS NULL`, fileID, apperror.ProcessingFailed)

	return err
}

// GetFile возвращает активный или soft-deleted файл по UUID.
func (r *Postgres) GetFile(ctx context.Context, id string) (domain.File, error) {
	row := r.pool.QueryRow(ctx, `
        SELECT id, owner_user_id, purpose, visibility, original_name, declared_content_type,
               detected_content_type, size_bytes, checksum_sha256, status, failure_code,
               created_at, updated_at, deleted_at
          FROM files WHERE id = $1`, id)
	file, err := scanFile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, apperror.New(apperror.FileNotFound, "файл не найден", http.StatusNotFound)
	}
	if err != nil {
		return domain.File{}, fmt.Errorf("получить файл: %w", err)
	}

	return file, nil
}

// GetUploadSession возвращает upload-сессию файла.
func (r *Postgres) GetUploadSession(ctx context.Context, fileID string) (domain.UploadSession, error) {
	session, err := scanUploadSession(r.pool.QueryRow(ctx, `
        SELECT file_id, mode, bucket, object_key, multipart_id, expected_size,
               checksum_sha256, content_type, expires_at, completed_at, part_size
          FROM upload_sessions WHERE file_id = $1`, fileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UploadSession{}, apperror.New(apperror.FileNotFound, "upload-сессия не найдена", http.StatusNotFound)
	}
	if err != nil {
		return domain.UploadSession{}, fmt.Errorf("получить upload-сессию: %w", err)
	}

	return session, nil
}

func scanUploadSession(row scanner) (domain.UploadSession, error) {
	var session domain.UploadSession
	err := row.Scan(
		&session.FileID, &session.Mode, &session.Bucket, &session.ObjectKey,
		&session.MultipartID, &session.ExpectedSize, &session.Checksum,
		&session.ContentType, &session.ExpiresAt, &session.CompletedAt, &session.PartSize,
	)

	return session, err
}

// GetObjectKey возвращает физический ключ готового blob.
func (r *Postgres) GetObjectKey(ctx context.Context, fileID, variant string) (string, error) {
	var key string
	var err error
	if variant == "" || variant == "original" {
		err = r.pool.QueryRow(ctx, `
            SELECT b.object_key FROM files f JOIN blobs b ON b.id=f.blob_id
             WHERE f.id=$1 AND f.status='ready' AND b.deleted_at IS NULL
               AND b.scan_status='clean' AND (b.scan_verified OR $2)`, fileID, r.allowUnscannedLocal).Scan(&key)
	} else {
		err = r.pool.QueryRow(ctx, `
            SELECT v.object_key FROM files f
              JOIN blobs b ON b.id=f.blob_id
              JOIN blob_variants v ON v.blob_id=b.id AND v.name=$2
             WHERE f.id=$1 AND f.status='ready' AND b.deleted_at IS NULL
               AND b.scan_status='clean' AND (b.scan_verified OR $3)`, fileID, variant, r.allowUnscannedLocal).Scan(&key)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apperror.New(apperror.FileNotReady, "файл ещё не готов", http.StatusConflict)
	}
	if err != nil {
		return "", fmt.Errorf("получить object key: %w", err)
	}

	return key, nil
}

// CompleteUpload locks the file until the bounded storage check and the state transition finish.
func (r *Postgres) CompleteUpload(ctx context.Context, fileID string, verify func(context.Context, domain.UploadSession) (string, error)) (domain.File, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.File{}, fmt.Errorf("начать complete upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	file, err := scanFile(tx.QueryRow(ctx, `
        SELECT id,owner_user_id,purpose,visibility,original_name,declared_content_type,
               detected_content_type,size_bytes,checksum_sha256,status,failure_code,created_at,updated_at,deleted_at
          FROM files WHERE id=$1 FOR UPDATE`, fileID))
	if err != nil {
		return domain.File{}, fmt.Errorf("lock complete upload: %w", err)
	}
	if file.FailureCode == apperror.UploadExpired {
		return domain.File{}, apperror.New(apperror.UploadExpired, "upload-сессия истекла", http.StatusGone)
	}
	if file.Status != domain.StatusPendingUpload {
		return file, nil
	}
	session, err := scanUploadSession(tx.QueryRow(ctx, `
        SELECT file_id,mode,bucket,object_key,multipart_id,expected_size,checksum_sha256,content_type,expires_at,completed_at,part_size
          FROM upload_sessions WHERE file_id=$1 AND expires_at>statement_timestamp()`, fileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, apperror.New(apperror.UploadExpired, "upload-сессия истекла", http.StatusGone)
	}
	if err != nil {
		return domain.File{}, err
	}
	detectedType, err := verify(ctx, session)
	if err != nil {
		return domain.File{}, err
	}
	err = tx.QueryRow(ctx, `
        UPDATE files
           SET status = 'quarantined', detected_content_type = $2, size_bytes = $3,
               checksum_sha256 = $4, updated_at = NOW()
         WHERE id = $1 RETURNING updated_at`, fileID, detectedType, session.ExpectedSize, session.Checksum).Scan(&file.UpdatedAt)
	if err != nil {
		return domain.File{}, fmt.Errorf("обновить статус файла: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE upload_sessions SET completed_at = NOW() WHERE file_id = $1`, fileID); err != nil {
		return domain.File{}, fmt.Errorf("завершить upload-сессию: %w", err)
	}
	if _, err := tx.Exec(ctx, `
            INSERT INTO media_jobs (id, file_id, kind) VALUES ($1,$2,'scan_and_process')`, uuid.NewString(), fileID); err != nil {
		return domain.File{}, fmt.Errorf("создать media job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
            INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload)
            VALUES ($1,'file',$2,'file.uploaded',jsonb_build_object('file_id',$2::uuid::text))`, uuid.NewString(), fileID); err != nil {
		return domain.File{}, fmt.Errorf("создать outbox event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.File{}, fmt.Errorf("зафиксировать complete upload: %w", err)
	}
	file.Status, file.DetectedContentType = domain.StatusQuarantined, detectedType
	file.SizeBytes, file.ChecksumSHA256 = session.ExpectedSize, session.Checksum

	return file, nil
}

// ListFiles возвращает страницу файлов владельца.
func (r *Postgres) ListFiles(ctx context.Context, ownerID, status string, limit, offset int) (domain.FileList, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id, owner_user_id, purpose, visibility, original_name, declared_content_type,
               detected_content_type, size_bytes, checksum_sha256, status, failure_code,
               created_at, updated_at, deleted_at
          FROM files
         WHERE owner_user_id = $1 AND deleted_at IS NULL AND ($2 = '' OR status = $2)
         ORDER BY created_at DESC LIMIT $3 OFFSET $4`, ownerID, status, limit, offset)
	if err != nil {
		return domain.FileList{}, fmt.Errorf("получить список файлов: %w", err)
	}
	defer rows.Close()
	items := make([]domain.File, 0)
	for rows.Next() {
		file, scanErr := scanFile(rows)
		if scanErr != nil {
			return domain.FileList{}, fmt.Errorf("прочитать файл списка: %w", scanErr)
		}
		items = append(items, file)
	}
	if err := rows.Err(); err != nil {
		return domain.FileList{}, fmt.Errorf("обойти список файлов: %w", err)
	}

	return domain.FileList{Items: items, Limit: limit, Offset: offset}, nil
}

// SoftDeleteFile помечает файл удалённым и создаёт idempotent GC job/outbox.
func (r *Postgres) SoftDeleteFile(ctx context.Context, id string, purgeAfter time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать soft delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var found bool
	if err = tx.QueryRow(ctx, `SELECT true FROM files WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&found); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("заблокировать файл для удаления: %w", err)
	}
	var bound bool
	if bindingErr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM file_bindings WHERE file_id=$1 AND deleted_at IS NULL)`, id).Scan(&bound); bindingErr != nil {
		return fmt.Errorf("проверить bindings: %w", bindingErr)
	}
	if bound {
		return apperror.New(apperror.FileInUse, "файл используется другой сущностью", http.StatusConflict)
	}
	command, err := tx.Exec(ctx, `
        UPDATE files SET status='deleted', deleted_at=NOW(), purge_after=$2, updated_at=NOW()
         WHERE id=$1 AND deleted_at IS NULL`, id, purgeAfter)
	if err != nil {
		return fmt.Errorf("пометить файл удалённым: %w", err)
	}
	if command.RowsAffected() > 0 {
		_, err = tx.Exec(ctx, `
            INSERT INTO media_jobs (id,file_id,kind,available_at) VALUES ($1,$2,'purge',$3)`, uuid.NewString(), id, purgeAfter)
		if err != nil {
			return fmt.Errorf("создать purge job: %w", err)
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO outbox_events (id,aggregate_type,aggregate_id,event_type,payload)
            VALUES ($1,'file',$2::uuid,'file.deleted',jsonb_build_object('file_id',($2::uuid)::text))`, uuid.NewString(), id)
		if err != nil {
			return fmt.Errorf("создать delete event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать soft delete: %w", err)
	}

	return nil
}

// ResolvePublicFiles одним запросом возвращает ключи готовых публичных WebP.
func (r *Postgres) ResolvePublicFiles(ctx context.Context, ids []string, variants []string) ([]domain.PublicFile, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT f.id, v.name, v.object_key
          FROM files f
          JOIN blobs b ON b.id=f.blob_id
          JOIN blob_variants v ON v.blob_id=b.id
         WHERE f.id=ANY($1::uuid[]) AND f.status='ready' AND f.visibility='public'
           AND f.purpose IN ('avatar','catalog_logo','content_image') AND b.visibility='public'
           AND f.deleted_at IS NULL AND b.deleted_at IS NULL AND v.name=ANY($2::text[])
           AND b.scan_status='clean' AND b.scan_verified
           AND v.format='webp' AND v.object_key LIKE $3`, ids, variants, domain.PublicVariantPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("разрешить public media files: %w", err)
	}

	return collectPublicPreviews(rows, ids)
}

func collectPublicPreviews(rows pgx.Rows, ids []string) ([]domain.PublicFile, error) {
	defer rows.Close()
	byID := make(map[string]*domain.PublicFile, len(ids))
	for rows.Next() {
		var fileID, variant, key string
		if err := rows.Scan(&fileID, &variant, &key); err != nil {
			return nil, fmt.Errorf("прочитать public media URL: %w", err)
		}
		item := byID[fileID]
		if item == nil {
			item = &domain.PublicFile{
				FileID: fileID,
				URLs:   make(map[string]string),
			}
			byID[fileID] = item
		}
		item.URLs[variant] = key
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обойти public media URLs: %w", err)
	}
	result := make([]domain.PublicFile, 0, len(byID))
	for _, id := range ids {
		if item := byID[id]; item != nil {
			result = append(result, *item)
		}
	}

	return result, nil
}

// ReplaceAvatarBinding атомарно заменяет или снимает активную связь пользовательского аватара.
func (r *Postgres) ReplaceAvatarBinding(ctx context.Context, userID string, fileID *string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать avatar binding transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if fileID != nil {
		var exists bool
		err = tx.QueryRow(ctx, `
            SELECT true FROM files f JOIN blobs b ON b.id=f.blob_id
            WHERE f.id=$1 AND f.owner_user_id=$2 AND f.purpose='avatar' AND f.visibility='public'
              AND f.status='ready' AND f.deleted_at IS NULL AND b.deleted_at IS NULL
              AND b.scan_status='clean' AND b.scan_verified
              AND b.visibility='public'
              AND EXISTS (SELECT 1 FROM blob_variants v WHERE v.blob_id=b.id
                          AND v.format='webp' AND v.object_key LIKE $3)
            FOR UPDATE OF f`, *fileID, userID, domain.PublicVariantPrefix+"%").Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.FileNotReady, "аватар не готов или не принадлежит пользователю", http.StatusConflict)
		}
		if err != nil {
			return fmt.Errorf("проверить avatar file: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `
        UPDATE file_bindings SET deleted_at=NOW()
         WHERE service_name='users' AND entity_type='user_avatar' AND entity_id=$1
           AND deleted_at IS NULL AND ($2::uuid IS NULL OR file_id<>$2)`, userID, fileID)
	if err != nil {
		return fmt.Errorf("снять предыдущий avatar binding: %w", err)
	}
	if fileID != nil {
		_, err = tx.Exec(ctx, `
            INSERT INTO file_bindings (id,file_id,service_name,entity_type,entity_id,created_by)
            SELECT $1,$2,'users','user_avatar',$3,$3
             WHERE NOT EXISTS (
                SELECT 1 FROM file_bindings
                 WHERE file_id=$2 AND service_name='users' AND entity_type='user_avatar'
                   AND entity_id=$3 AND deleted_at IS NULL
             )`, uuid.NewString(), *fileID, userID)
		if err != nil {
			return fmt.Errorf("создать avatar binding: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать avatar binding: %w", err)
	}

	return nil
}

type scanner interface {
	Scan(...any) error
}

func scanFile(row scanner) (domain.File, error) {
	var file domain.File
	err := row.Scan(
		&file.ID, &file.OwnerUserID, &file.Purpose, &file.Visibility, &file.OriginalName,
		&file.DeclaredContentType, &file.DetectedContentType, &file.SizeBytes,
		&file.ChecksumSHA256, &file.Status, &file.FailureCode,
		&file.CreatedAt, &file.UpdatedAt, &file.DeletedAt,
	)

	return file, err
}
