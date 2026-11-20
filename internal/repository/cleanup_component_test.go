//go:build component

package repository

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/service"
	"github.com/overmindv/media/internal/testutil"
)

func TestUploadCompletionSerializesWithCleanup(t *testing.T) {
	repo := componentRepository(t)
	file, session := uploadFixture()
	if err := repo.CreateUpload(t.Context(), file, session, testutil.UploadQuota()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	var checks atomic.Int32
	verify := func(ctx context.Context, _ domain.UploadSession) (string, error) {
		if checks.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "image/png", nil
		}
	}
	for range 2 {
		go func() {
			_, err := repo.CompleteUpload(ctx, file.ID, verify)
			results <- err
		}()
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("complete did not acquire the upload")
	}
	if err := repo.ScheduleUploadCleanup(ctx, session.ExpiresAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if checks.Load() != 1 {
		t.Error("duplicate complete reached storage", checks.Load())
	}
	if err := repo.ScheduleUploadCleanup(ctx, session.ExpiresAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var scan, cleanup int
	err := repo.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE kind='scan_and_process'),
        count(*) FILTER(WHERE kind='cleanup_upload') FROM media_jobs WHERE file_id=$1`, file.ID).Scan(&scan, &cleanup)
	if err != nil || scan != 1 || cleanup != 0 {
		t.Error("cleanup touched an active upload", scan, cleanup, err)
	}
}

func TestExpiredMultipartQueuesOnlyOneCleanup(t *testing.T) {
	repo := componentRepository(t)
	file, session := uploadFixture()
	session.ExpiresAt = time.Now().Add(-time.Hour)
	session.Mode, session.MultipartID = domain.UploadModeMultipart, "fixture-upload"
	if err := repo.CreateUpload(t.Context(), file, session, testutil.UploadQuota()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repo.ScheduleUploadCleanup(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := repo.ClaimJob(t.Context(), false); err != nil || found {
		t.Fatal("disabled cleanup claimed a job", found, err)
	}
	job, found, err := repo.ClaimJob(t.Context(), true)
	if err != nil || !found || job.Kind != "cleanup_upload" {
		t.Fatal("expected the expired session job", job.Kind, found, err)
	}
	target, err := repo.PrepareUploadCleanup(t.Context(), job)
	if err != nil || target.MultipartID != session.MultipartID || target.Bucket != session.Bucket || target.FileID != file.ID {
		t.Error("wrong cleanup target", target.FileID, err)
	}
	_, err = repo.CompleteUpload(t.Context(), file.ID, func(context.Context, domain.UploadSession) (string, error) {
		t.Error("expired upload reached storage")
		return "", nil
	})
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Code != apperror.UploadExpired {
		t.Error("expired session should be gone", err)
	}
	if err := repo.CompleteUploadCleanup(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.ClaimJob(t.Context(), true); err != nil || found {
		t.Error("cleanup ran more than once", found, err)
	}
}

func TestCleanupDoesNotTouchAdoptedKeysOrLiveLeases(t *testing.T) {
	repo := componentRepository(t)
	ctx := t.Context()
	active := completeFixture(t, repo)
	job := claimForFile(t, repo, active.ID, "scan_and_process")
	const staged = "originals/staged"
	if err := repo.TrackObject(ctx, job, staged); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, "UPDATE media_job_objects SET created_at=NOW()-interval '2 hours'"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, "UPDATE media_jobs SET updated_at=NOW()-interval '2 hours' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)
	if keys, err := repo.StagedCleanupTargets(ctx, cutoff); err != nil || len(keys) != 0 {
		t.Error("live key was eligible for deletion", keys, err)
	}
	publishFixture(t, repo, job, active, "originals/adopted")
	if _, err := repo.pool.Exec(ctx, "UPDATE media_jobs SET updated_at=NOW()-interval '2 hours' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	if keys, err := repo.StagedCleanupTargets(ctx, cutoff); err != nil || len(keys) != 1 || keys[0] != staged {
		t.Error("only the unadopted key should be selected", keys, err)
	}
	var orphanID string
	err := repo.pool.QueryRow(ctx, `INSERT INTO blobs(id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key,scan_status,created_at)
        VALUES(gen_random_uuid(),'orphan',4,'private','image/png','originals/orphan','clean',NOW()-interval '8 days') RETURNING id`).Scan(&orphanID)
	if err != nil {
		t.Fatal(err)
	}
	cutoff = time.Now().Add(-7 * 24 * time.Hour)
	blobs, err := repo.UnboundCleanupTargets(ctx, cutoff)
	if err != nil || len(blobs) != 1 || blobs[0].ID != orphanID || len(blobs[0].Keys) != 1 {
		t.Fatal("only the unbound blob should be selected", blobs, err)
	}
	if err := repo.CompleteUnboundCleanup(ctx, orphanID); err != nil {
		t.Fatal(err)
	}
	if blobs, err := repo.UnboundCleanupTargets(ctx, cutoff); err != nil || len(blobs) != 0 {
		t.Error("cleaned blob was selected again", blobs, err)
	}
}

func TestRejectedOriginalBecomesEligibleAfterUploadCleanup(t *testing.T) {
	repo := componentRepository(t)
	ctx := t.Context()
	file := completeFixture(t, repo)
	job := claimForFile(t, repo, file.ID, "scan_and_process")
	const key = "originals/rejected"
	if err := repo.MarkProcessing(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := repo.TrackObject(ctx, job, key); err != nil {
		t.Fatal(err)
	}
	blob, _, err := repo.LinkCleanBlob(ctx, job, file, file.DeclaredContentType, key, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RejectFile(ctx, job, apperror.UnsupportedMediaType); err != nil {
		t.Fatal(err)
	}
	if err := repo.ScheduleUploadCleanup(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := repo.UnboundCleanupTargets(ctx, time.Now().Add(time.Minute))
	if err != nil || len(items) != 1 || items[0].ID != blob.ID || len(items[0].Keys) != 1 || items[0].Keys[0] != key {
		t.Error("rejected original should become an unbound blob", items, err)
	}
}

func BenchmarkDuplicateComplete(b *testing.B) {
	repo := New(testutil.Postgres(b), false)
	file, session := uploadFixture()
	file.Status = domain.StatusQuarantined
	if err := repo.CreateUpload(b.Context(), file, session, testutil.UploadQuota()); err != nil {
		b.Fatal(err)
	}
	b.Run("previous_reads", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := repo.GetFile(b.Context(), file.ID); err != nil {
				b.Fatal(err)
			}
			if _, err := repo.GetUploadSession(b.Context(), file.ID); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("duplicate", func(b *testing.B) {
		b.ReportAllocs()
		media := service.New(repo, nil, config.Config{})
		for b.Loop() {
			if _, err := media.CompleteUpload(b.Context(), file.ID, nil, domain.Actor{UserID: file.OwnerUserID}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
