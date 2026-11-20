package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/inventory"
)

const inventoryOriginalKeys = `
    SELECT b.object_key COLLATE "C" AS key,b.id AS blob_id,b.created_at,b.cleaned_at
      FROM blobs b WHERE b.object_key COLLATE "C">$1 AND b.object_key COLLATE "C"<$2
     ORDER BY b.object_key COLLATE "C" LIMIT $3`

const inventoryVariantKeys = `
    SELECT v.object_key COLLATE "C" AS key,v.blob_id,v.created_at,b.cleaned_at
      FROM blob_variants v JOIN blobs b ON b.id=v.blob_id
     WHERE v.object_key COLLATE "C">$1 AND v.object_key COLLATE "C"<$2
     ORDER BY v.object_key COLLATE "C" LIMIT $3`

const inventoryDeliveryTail = `
    ), staged AS (
        SELECT o.object_key COLLATE "C" AS key FROM media_job_objects o
         WHERE o.object_key COLLATE "C">$1 AND o.object_key COLLATE "C"<$2
         ORDER BY o.object_key COLLATE "C" LIMIT $3
    ), candidates AS (
        SELECT key, CASE WHEN EXISTS (
            SELECT 1 FROM files f WHERE f.blob_id=c.blob_id AND f.deleted_at IS NULL AND f.status='ready'
        ) THEN c.created_at END AS expected_since,cleaned_at FROM canonical c
        UNION ALL
        SELECT key,NULL,NULL FROM staged
    )
    SELECT key,MIN(expected_since),MAX(cleaned_at) FROM candidates
     GROUP BY key ORDER BY key LIMIT $3`

const inventoryUploads = `
    WITH candidates AS MATERIALIZED (
        SELECT s.object_key COLLATE "C" AS key,s.file_id,s.completed_at,s.cleaned_at FROM upload_sessions s
         WHERE s.bucket=$4 AND s.object_key COLLATE "C">$1 AND s.object_key COLLATE "C"<$2
         ORDER BY s.object_key COLLATE "C" LIMIT $3
    )
    SELECT c.key, CASE WHEN f.status IN ('quarantined','processing') AND f.deleted_at IS NULL
              THEN c.completed_at END, c.cleaned_at
      FROM candidates c JOIN files f ON f.id=c.file_id ORDER BY c.key`

func InventoryPages(tx pgx.Tx, bucket, prefix string) (inventory.Page, error) {
	var query string
	switch prefix {
	case "originals/":
		query = "WITH canonical AS MATERIALIZED (" + inventoryOriginalKeys + inventoryDeliveryTail
	case "variants/":
		query = "WITH canonical AS MATERIALIZED (" + inventoryVariantKeys + inventoryDeliveryTail
	case "uploads/":
		query = inventoryUploads
	default:
		return nil, fmt.Errorf("unsupported inventory prefix")
	}
	upper := prefix[:len(prefix)-1] + "0"
	return func(ctx context.Context, after string, size int) ([]inventory.Entry, bool, error) {
		args := []any{after, upper, size}
		if prefix == "uploads/" {
			args = append(args, bucket)
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, false, err
		}
		entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (inventory.Entry, error) {
			var entry inventory.Entry
			var expected, cleaned *time.Time
			err := row.Scan(&entry.Key, &expected, &cleaned)
			if expected != nil {
				entry.ExpectedSince = *expected
			}
			if cleaned != nil {
				entry.CleanedAt = *cleaned
			}
			return entry, err
		})
		return entries, len(entries) == size, err
	}, nil
}
