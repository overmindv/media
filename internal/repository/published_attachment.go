package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

func (r *Postgres) PublishedAttachmentKey(ctx context.Context, ref domain.PublishedAttachmentRef) (string, error) {
	const query = `
		SELECT CASE WHEN f.purpose = 'content_image' THEN COALESCE(preview.object_key, '')
		       WHEN b.object_key LIKE 'originals/verified/%' THEN b.object_key ELSE '' END
		FROM resource_binding_versions v
		JOIN file_bindings fb ON fb.entity_id = v.entity_id AND fb.entity_type = v.scope
		JOIN files f ON f.id = fb.file_id
		JOIN blobs b ON b.id = f.blob_id
		LEFT JOIN LATERAL (
			SELECT p.object_key FROM blob_variants p
			WHERE f.purpose = 'content_image' AND p.blob_id = b.id AND p.format = 'webp'
			  AND p.name IN ('w128', 'w320', 'w768', 'w1440')
			  AND p.object_key LIKE CASE WHEN b.visibility = 'public'
			      THEN $4 ELSE 'variants/private/%' END
			  AND p.object_key LIKE '%.webp'
			ORDER BY p.width DESC, p.name LIMIT 1
		) preview ON true
		WHERE v.scope = 'content_revision' AND v.entity_id = $2 AND v.resource_id = $1 AND v.version > 0
		  AND fb.service_name = 'content' AND fb.file_id = $3 AND fb.deleted_at IS NULL
		  AND f.purpose IN ('content_image', 'attachment', 'archive')
		  AND f.status = 'ready' AND f.deleted_at IS NULL AND f.visibility = b.visibility
		  AND b.scan_status = 'clean' AND b.scan_verified AND b.deleted_at IS NULL`
	var key string
	err := r.pool.QueryRow(ctx, query, ref.MaterialID, ref.RevisionID, ref.FileID, domain.PublicVariantPrefix+"%").Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && key == "") {
		return "", apperror.New(apperror.FileNotFound, "вложение недоступно", http.StatusNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("find published attachment: %w", err)
	}

	return key, nil
}
