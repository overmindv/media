//go:build component

package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/service"
	"github.com/overmindv/media/internal/testutil"
)

func TestWorkerRetriesMultipartCleanup(t *testing.T) {
	ctx := t.Context()
	pool := testutil.Postgres(t)
	repo := repository.New(pool, false)
	id := uuid.NewString()
	now := time.Now()
	file := domain.File{
		ID: id, OwnerUserID: uuid.NewString(), Purpose: domain.PurposeAttachment, Visibility: domain.VisibilityPrivate,
		OriginalName: "fixture", DeclaredContentType: "application/pdf", SizeBytes: 1, ChecksumSHA256: "fixture",
		Status: domain.StatusPendingUpload, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
	session := domain.UploadSession{
		FileID: id, Mode: domain.UploadModeMultipart, Bucket: "quarantine", ObjectKey: "uploads/" + id, MultipartID: "fixture-upload",
		ExpectedSize: 1, Checksum: "fixture", ContentType: "application/pdf", ExpiresAt: now.Add(-time.Hour),
	}
	if err := repo.CreateUpload(ctx, file, session, testutil.UploadQuota()); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("fixture storage outage")
	objects := &cleanupStorage{abortErr: unavailable}
	cfg := config.Config{S3: config.S3{QuarantineBucket: "quarantine", MediaBucket: "media"},
		EnableUploadCleanup: true, DeleteRetention: 7 * 24 * time.Hour, UploadTTL: 15 * time.Minute}
	processor := NewProcessor(repo, objects, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := processor.runOne(ctx); !errors.Is(err, unavailable) {
		t.Fatal("expected a retryable abort failure", err)
	}
	checkScanState(t, repo, id, domain.StatusRejected, apperror.UploadExpired)
	if len(objects.calls) != 1 || objects.calls[0] != "abort:quarantine/"+session.ObjectKey {
		t.Error("cleanup proceeded after a failed abort", objects.calls)
	}
	tag, err := pool.Exec(ctx, "UPDATE media_jobs SET available_at=NOW() WHERE file_id=$1 AND kind='cleanup_upload' AND status='pending'", id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("cleanup did not remain pending", err)
	}
	objects.abortErr = nil
	if err := processor.runOne(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"abort:quarantine/" + session.ObjectKey, "abort:quarantine/" + session.ObjectKey, "delete:quarantine/" + session.ObjectKey}
	if !slices.Equal(objects.calls, want) {
		t.Error("only the expired upload may be removed", objects.calls)
	}
	var cleaned bool
	if err := pool.QueryRow(ctx, "SELECT cleaned_at IS NOT NULL FROM upload_sessions WHERE file_id=$1", id).Scan(&cleaned); err != nil || !cleaned {
		t.Error("successful cleanup was not acknowledged", err)
	}
}

type cleanupStorage struct {
	service.ObjectStorage
	abortErr error
	calls    []string
}

func (s *cleanupStorage) AbortMultipart(_ context.Context, bucket, key, _ string) error {
	s.calls = append(s.calls, "abort:"+bucket+"/"+key)
	return s.abortErr
}

func (s *cleanupStorage) Delete(_ context.Context, bucket, key string) error {
	s.calls = append(s.calls, "delete:"+bucket+"/"+key)
	return nil
}
