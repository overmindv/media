//go:build component

package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/parker"
)

// Each test owns a schema, including its Goose history, on the supplied test database.
func componentRepository(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("COMPONENT_TEST_DSN")
	if dsn == "" {
		t.Fatal("COMPONENT_TEST_DSN is required for component tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "media_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	migrationDSN := stdlib.RegisterConnConfig(config.ConnConfig)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(migrationDSN) })
	if err := parker.RunMigrate(filepath.Join("..", "..", "migrations"), migrationDSN, "up"); err != nil {
		t.Fatalf("apply service migrations: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

func uploadFixture() (domain.File, domain.UploadSession) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	file := domain.File{
		ID: uuid.NewString(), OwnerUserID: uuid.NewString(),
		Purpose: domain.PurposeAvatar, Visibility: domain.VisibilityPrivate,
		OriginalName: "avatar.png", DeclaredContentType: "image/png", SizeBytes: 1024,
		ChecksumSHA256: "test-checksum", Status: domain.StatusPendingUpload,
		CreatedAt: now, UpdatedAt: now,
	}
	session := domain.UploadSession{
		FileID: file.ID, Mode: domain.UploadModeSingle, Bucket: "media-test",
		ObjectKey: "uploads/" + file.ID, ExpectedSize: file.SizeBytes,
		Checksum: file.ChecksumSHA256, ContentType: file.DeclaredContentType,
		ExpiresAt: now.Add(time.Hour),
	}
	return file, session
}

func TestPostgresUploadCompletionIsIdempotent(t *testing.T) {
	repo := componentRepository(t)
	ctx := context.Background()
	file, session := uploadFixture()
	if err := repo.CreateUpload(ctx, file, session); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetFile(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != file.ID || stored.OwnerUserID != file.OwnerUserID || stored.Status != domain.StatusPendingUpload {
		t.Fatalf("unexpected stored file: %+v", stored)
	}
	size, err := repo.ActiveSize(ctx, file.OwnerUserID)
	if err != nil || size != 1024 {
		t.Fatalf("active size = %d, err = %v; want 1024", size, err)
	}
	for range 2 {
		completed, err := repo.CompleteUpload(ctx, file.ID, "image/png", 1024, "test-checksum")
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status != domain.StatusQuarantined || completed.DetectedContentType != "image/png" {
			t.Fatalf("unexpected completed file: %+v", completed)
		}
	}
	storedSession, err := repo.GetUploadSession(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedSession.CompletedAt == nil || storedSession.ObjectKey != session.ObjectKey {
		t.Fatalf("unexpected upload session: %+v", storedSession)
	}
	var events int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id=$1 AND event_type='file.uploaded' AND payload->>'file_id'=$2`, file.ID, file.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("uploaded events = %d; want 1", events)
	}
	job, found, err := repo.ClaimJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim upload job: found=%v, err=%v", found, err)
	}
	if job.FileID != file.ID || job.Kind != "scan_and_process" || job.Attempts != 1 {
		t.Fatalf("unexpected upload job: %+v", job)
	}
	if _, found, err := repo.ClaimJob(ctx); err != nil || found {
		t.Fatalf("duplicate upload job: found=%v, err=%v", found, err)
	}
}

func TestPostgresCreateUploadRollsBackWithoutSession(t *testing.T) {
	repo := componentRepository(t)
	ctx := context.Background()
	file, session := uploadFixture()
	// A session pointing at a missing file must roll back the preceding file insert.
	session.FileID = uuid.NewString()
	if err := repo.CreateUpload(ctx, file, session); err == nil {
		t.Fatal("expected upload session foreign-key violation")
	}
	var files int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE id=$1`, file.ID).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("files after rollback = %d; want 0", files)
	}
}
