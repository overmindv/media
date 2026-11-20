//go:build component

package repository

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/testutil"
)

func TestInventoryPagesForVariantsAndUploadBuckets(t *testing.T) {
	repo := componentRepository(t)
	ctx := t.Context()
	seedPreviews(t, repo, 2)
	file, session := uploadFixture()
	if err := repo.CreateUpload(ctx, file, session, testutil.UploadQuota()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteUpload(ctx, file.ID, func(context.Context, domain.UploadSession) (string, error) {
		return "image/png", nil
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := repo.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	variants, err := InventoryPages(tx, "media", "variants/")
	if err != nil {
		t.Fatal(err)
	}
	first, more, err := variants(ctx, "variants/", 1)
	if err != nil || len(first) != 1 || !more || first[0].ExpectedSince.IsZero() {
		t.Fatal("expected one required variant", first, more, err)
	}
	second, _, err := variants(ctx, first[0].Key, 1)
	if err != nil || len(second) != 1 || second[0].Key <= first[0].Key {
		t.Error("variant cursor did not advance", second, err)
	}
	for _, bucket := range []string{session.Bucket, "other"} {
		uploads, err := InventoryPages(tx, bucket, "uploads/")
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := uploads(ctx, "uploads/", 10)
		if err != nil {
			t.Fatal(err)
		}
		if bucket == session.Bucket {
			if len(result) != 1 || result[0].Key != session.ObjectKey || result[0].ExpectedSince.IsZero() {
				t.Error("completed upload should be expected while scanning", result)
			}
		} else if len(result) != 0 {
			t.Error("upload from a different bucket appeared", result)
		}
	}
}
