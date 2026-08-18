package repository

import (
	"context"
	"encoding/json"
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
	pool *pgxpool.Pool
}

// New создаёт PostgreSQL repository Media.
func New(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// Ping проверяет соединение с PostgreSQL.
func (r *Postgres) Ping(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return nil
}

// ActiveSize возвращает суммарный заявленный размер незавершённых и готовых файлов пользователя.
func (r *Postgres) ActiveSize(ctx context.Context, ownerUserID string) (int64, error) {
	var size int64
	if err := r.pool.QueryRow(ctx, `
        SELECT COALESCE(SUM(size_bytes), 0)
          FROM files
         WHERE owner_user_id = $1
           AND status NOT IN ('rejected', 'deleted')`, ownerUserID).Scan(&size); err != nil {
		return 0, fmt.Errorf("получить active media size: %w", err)
	}

	return size, nil
}

// CreateUpload атомарно сохраняет файл и upload-сессию.
func (r *Postgres) CreateUpload(ctx context.Context, file domain.File, session domain.UploadSession) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("начать транзакцию upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
        INSERT INTO files (
            id, owner_user_id, purpose, visibility, original_name, declared_content_type,
            size_bytes, checksum_sha256, status, created_at, updated_at
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		file.ID, file.OwnerUserID, file.Purpose, file.Visibility, file.OriginalName,
		file.DeclaredContentType, file.SizeBytes, file.ChecksumSHA256, file.Status,
		file.CreatedAt, file.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("вставить файл: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO upload_sessions (
            file_id, mode, bucket, object_key, multipart_id, expected_size,
            checksum_sha256, content_type, expires_at
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		session.FileID, session.Mode, session.Bucket, session.ObjectKey, session.MultipartID,
		session.ExpectedSize, session.Checksum, session.ContentType, session.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("вставить upload-сессию: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать upload-транзакцию: %w", err)
	}

	return nil
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
	var session domain.UploadSession
	err := r.pool.QueryRow(ctx, `
        SELECT file_id, mode, bucket, object_key, multipart_id, expected_size,
               checksum_sha256, content_type, expires_at, completed_at
          FROM upload_sessions WHERE file_id = $1`, fileID).Scan(
		&session.FileID, &session.Mode, &session.Bucket, &session.ObjectKey,
		&session.MultipartID, &session.ExpectedSize, &session.Checksum,
		&session.ContentType, &session.ExpiresAt, &session.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UploadSession{}, apperror.New(apperror.FileNotFound, "upload-сессия не найдена", http.StatusNotFound)
	}
	if err != nil {
		return domain.UploadSession{}, fmt.Errorf("получить upload-сессию: %w", err)
	}

	return session, nil
}

// GetObjectKey возвращает физический ключ готового blob.
func (r *Postgres) GetObjectKey(ctx context.Context, fileID, variant string) (string, error) {
	var key string
	var err error
	if variant == "" || variant == "original" {
		err = r.pool.QueryRow(ctx, `
            SELECT b.object_key FROM files f JOIN blobs b ON b.id=f.blob_id
             WHERE f.id=$1 AND f.status='ready' AND b.deleted_at IS NULL`, fileID).Scan(&key)
	} else {
		err = r.pool.QueryRow(ctx, `
            SELECT v.object_key FROM files f
              JOIN blobs b ON b.id=f.blob_id
              JOIN blob_variants v ON v.blob_id=b.id AND v.name=$2
             WHERE f.id=$1 AND f.status='ready' AND b.deleted_at IS NULL`, fileID, variant).Scan(&key)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apperror.New(apperror.FileNotReady, "файл ещё не готов", http.StatusConflict)
	}
	if err != nil {
		return "", fmt.Errorf("получить object key: %w", err)
	}

	return key, nil
}

// CompleteUpload переводит файл в quarantine и атомарно создаёт job/outbox.
func (r *Postgres) CompleteUpload(ctx context.Context, fileID, detectedType string, size int64, checksum string) (domain.File, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.File{}, fmt.Errorf("начать complete upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `
        UPDATE files
           SET status = 'quarantined', detected_content_type = $2, size_bytes = $3,
               checksum_sha256 = $4, updated_at = NOW()
         WHERE id = $1 AND status = 'pending_upload'`, fileID, detectedType, size, checksum)
	if err != nil {
		return domain.File{}, fmt.Errorf("обновить статус файла: %w", err)
	}
	if command.RowsAffected() > 0 {
		_, err = tx.Exec(ctx, `UPDATE upload_sessions SET completed_at = NOW() WHERE file_id = $1`, fileID)
		if err != nil {
			return domain.File{}, fmt.Errorf("завершить upload-сессию: %w", err)
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO media_jobs (id, file_id, kind) VALUES ($1,$2,'scan_and_process')`, uuid.NewString(), fileID)
		if err != nil {
			return domain.File{}, fmt.Errorf("создать media job: %w", err)
		}
		payload, marshalErr := json.Marshal(map[string]string{"file_id": fileID})
		if marshalErr != nil {
			return domain.File{}, fmt.Errorf("сериализовать outbox payload: %w", marshalErr)
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload)
            VALUES ($1,'file',$2,'file.uploaded',$3)`, uuid.NewString(), fileID, payload)
		if err != nil {
			return domain.File{}, fmt.Errorf("создать outbox event: %w", err)
		}
	}
	row := tx.QueryRow(ctx, `
        SELECT id, owner_user_id, purpose, visibility, original_name, declared_content_type,
               detected_content_type, size_bytes, checksum_sha256, status, failure_code,
               created_at, updated_at, deleted_at FROM files WHERE id = $1`, fileID)
	file, err := scanFile(row)
	if err != nil {
		return domain.File{}, fmt.Errorf("прочитать завершённый файл: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.File{}, fmt.Errorf("зафиксировать complete upload: %w", err)
	}

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
	var bindings int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM file_bindings WHERE file_id=$1 AND deleted_at IS NULL`, id).Scan(&bindings); err != nil {
		return fmt.Errorf("проверить bindings: %w", err)
	}
	if bindings > 0 {
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
            VALUES ($1,'file',$2,'file.deleted',jsonb_build_object('file_id',$2::text))`, uuid.NewString(), id)
		if err != nil {
			return fmt.Errorf("создать delete event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("зафиксировать soft delete: %w", err)
	}

	return nil
}

// ResolvePublicFiles одним запросом возвращает object keys готовых публичных файлов и вариантов.
func (r *Postgres) ResolvePublicFiles(ctx context.Context, ids []string, variants []string) ([]domain.PublicFile, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT file_id, variant_name, object_key
          FROM (
                SELECT f.id AS file_id, 'original'::text AS variant_name, b.object_key
                  FROM files f JOIN blobs b ON b.id=f.blob_id
                 WHERE f.id=ANY($1::uuid[]) AND f.status='ready' AND f.visibility='public'
                   AND f.deleted_at IS NULL AND b.deleted_at IS NULL
                UNION ALL
                SELECT f.id, v.name, v.object_key
                  FROM files f
                  JOIN blobs b ON b.id=f.blob_id
                  JOIN blob_variants v ON v.blob_id=b.id
                 WHERE f.id=ANY($1::uuid[]) AND f.status='ready' AND f.visibility='public'
                   AND f.deleted_at IS NULL AND b.deleted_at IS NULL AND v.name=ANY($2::text[])
          ) resolved`, ids, variants)
	if err != nil {
		return nil, fmt.Errorf("разрешить public media files: %w", err)
	}
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
            SELECT EXISTS(
                SELECT 1 FROM files
                 WHERE id=$1 AND owner_user_id=$2 AND purpose='avatar' AND visibility='public'
                   AND status='ready' AND deleted_at IS NULL
            )`, *fileID, userID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("проверить avatar file: %w", err)
		}
		if !exists {
			return apperror.New(apperror.FileNotReady, "аватар не готов или не принадлежит пользователю", http.StatusConflict)
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
