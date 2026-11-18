package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

func (r *Postgres) CheckResourceBindingFiles(ctx context.Context, binding domain.ResourceBinding) error {
	return checkResourceBindingFiles(ctx, r.pool, binding, false)
}

func (r *Postgres) ReplaceResourceBinding(ctx context.Context, binding domain.ResourceBinding) error {
	body, err := json.Marshal(binding.BindingFiles)
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(body)
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		const ensure = `
			INSERT INTO resource_binding_versions (scope, entity_id, resource_id, version)
			VALUES ($1, $2, $3, 0) ON CONFLICT DO NOTHING`
		if _, err := tx.Exec(ctx, ensure, binding.Scope, binding.EntityID, binding.ResourceID); err != nil {
			return fmt.Errorf("ensure resource binding version: %w", err)
		}
		const lock = `
			SELECT resource_id, version, request_hash FROM resource_binding_versions
			WHERE scope = $1 AND entity_id = $2 FOR UPDATE`
		var resourceID string
		var version int64
		var previous []byte
		if err := tx.QueryRow(ctx, lock, binding.Scope, binding.EntityID).Scan(&resourceID, &version, &previous); err != nil {
			return fmt.Errorf("lock resource binding version: %w", err)
		}
		if resourceID != binding.ResourceID || (binding.Version == version && !bytes.Equal(previous, fingerprint[:])) {
			return apperror.New(apperror.BindingVersionConflict, "версия привязки уже используется", http.StatusConflict)
		}
		if binding.Version <= version {
			return nil
		}
		if err := checkResourceBindingFiles(ctx, tx, binding, true); err != nil {
			return err
		}

		const remove = `
			UPDATE file_bindings SET deleted_at = statement_timestamp()
			WHERE service_name = $1 AND entity_type = $2 AND entity_id = $3
			  AND deleted_at IS NULL AND NOT (file_id = ANY($4::uuid[]))`
		service := binding.Scope.ServiceName()
		if _, err := tx.Exec(ctx, remove, service, binding.Scope, binding.EntityID, binding.FileIDs); err != nil {
			return fmt.Errorf("remove resource bindings: %w", err)
		}
		const attach = `
			INSERT INTO file_bindings (id, file_id, service_name, entity_type, entity_id, created_by)
			SELECT gen_random_uuid(), f.id, $1, $2, $3, $4 FROM unnest($5::uuid[]) AS f(id)
			ON CONFLICT (file_id, service_name, entity_type, entity_id) WHERE deleted_at IS NULL DO NOTHING`
		if len(binding.FileIDs) > 0 {
			if _, err := tx.Exec(ctx, attach, service, binding.Scope, binding.EntityID, binding.ActorUserID, binding.FileIDs); err != nil {
				return fmt.Errorf("attach resource files: %w", err)
			}
		}
		const advance = `
			UPDATE resource_binding_versions SET version = $3, request_hash = $4
			WHERE scope = $1 AND entity_id = $2`
		if _, err := tx.Exec(ctx, advance, binding.Scope, binding.EntityID, binding.Version, fingerprint[:]); err != nil {
			return fmt.Errorf("advance resource binding version: %w", err)
		}
		return nil
	})
}

func checkResourceBindingFiles(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, binding domain.ResourceBinding, lock bool) error {
	if len(binding.FileIDs) == 0 {
		return nil
	}
	query := `
		SELECT count(*) FROM (
		    SELECT f.id FROM files AS f JOIN blobs AS b ON b.id = f.blob_id
		    WHERE f.id = ANY($1::uuid[]) AND f.status = 'ready' AND f.deleted_at IS NULL
		      AND b.scan_status = 'clean' AND b.deleted_at IS NULL
		      AND CASE $2::text
		          WHEN 'catalog_logo' THEN f.purpose = 'catalog_logo' AND f.visibility = 'public' AND f.owner_user_id = $3
		          WHEN 'content_revision' THEN f.purpose IN ('content_image', 'attachment', 'archive') AND (
		              f.owner_user_id = $3 OR EXISTS (
		                  SELECT 1 FROM file_bindings AS source
		                  JOIN resource_binding_versions AS v ON v.scope = 'content_revision' AND v.entity_id = source.entity_id
		                  WHERE source.service_name = 'content' AND source.entity_type = 'content_revision'
		                    AND source.entity_id = $5 AND source.file_id = f.id AND source.deleted_at IS NULL
		                    AND v.resource_id = $4
		              )
		          )
		          ELSE false
		      END
		    ORDER BY f.id`
	if lock {
		query += ` FOR UPDATE OF f`
	}
	query += `) AS eligible`
	var count int
	err := q.QueryRow(ctx, query, binding.FileIDs, binding.Scope, binding.ActorUserID,
		optionalBindingID(binding.ResourceID), optionalBindingID(binding.InheritFromRevisionID)).Scan(&count)
	if err != nil {
		return fmt.Errorf("check resource files: %w", err)
	}
	if count != len(binding.FileIDs) {
		return apperror.New(apperror.FileNotReady, "файл недоступен для привязки", http.StatusConflict)
	}
	return nil
}

func optionalBindingID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
