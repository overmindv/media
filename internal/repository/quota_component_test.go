//go:build component

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/testutil"
)

func TestConcurrentIntentsCannotOversubscribeQuota(t *testing.T) {
	repo := componentRepository(t)
	owner := uuid.NewString()
	file, session := uploadFixture()
	quota := domain.UploadQuota{MaxBytes: file.SizeBytes, MaxMultipartSessions: 4}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, worker := range []*Postgres{repo, New(repo.pool, false)} {
		file, session := uploadFixture()
		file.OwnerUserID = owner
		go func() {
			<-start
			results <- worker.CreateUpload(t.Context(), file, session, quota)
		}()
	}
	close(start)
	accepted := 0
	for range 2 {
		err := <-results
		if err == nil {
			accepted++
		} else {
			requireQuotaError(t, err, apperror.FileTooLarge)
		}
	}
	if accepted != 1 {
		t.Error("accepted intents", accepted)
	}
	checkUsed(t, repo, owner, session.ExpectedSize)
}

func TestMultipartSlotsReleaseOnExpiryAndFailure(t *testing.T) {
	repo := componentRepository(t)
	owner := uuid.NewString()
	quota := domain.UploadQuota{MaxBytes: 4096, MaxMultipartSessions: 1}
	file, session := quotaUpload(owner, domain.UploadModeMultipart)
	if err := repo.CreateUpload(t.Context(), file, session, quota); err != nil {
		t.Fatal(err)
	}
	next, upload := quotaUpload(owner, domain.UploadModeMultipart)
	requireQuotaError(t, repo.CreateUpload(t.Context(), next, upload, quota), apperror.UploadLimitReached)

	if _, err := repo.pool.Exec(t.Context(), "UPDATE files SET upload_expires_at=NOW()-interval '1 second' WHERE id=$1", file.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateUpload(t.Context(), next, upload, quota); err != nil {
		t.Fatal("expired intent did not release slot:", err)
	}
	expired, err := repo.GetFile(t.Context(), file.ID)
	if err != nil || expired.Status != domain.StatusRejected || expired.FailureCode != apperror.UploadExpired {
		t.Error("unexpected expired file", expired.Status, expired.FailureCode, err)
	}
	checkUsed(t, repo, owner, upload.ExpectedSize)
	if err := repo.FailUploadIntent(t.Context(), next.ID); err != nil {
		t.Fatal(err)
	}
	requireQuotaError(t, repo.SetMultipartID(t.Context(), next.ID, "late-fixture"), apperror.UploadExpired)
	checkUsed(t, repo, owner, 0)
	newFile, newUpload := quotaUpload(owner, domain.UploadModeMultipart)
	if err := repo.CreateUpload(t.Context(), newFile, newUpload, quota); err != nil {
		t.Fatal("failed intent did not release slot:", err)
	}
	if err := repo.SetMultipartID(t.Context(), newFile.ID, "fixture-multipart"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SoftDeleteFile(t.Context(), newFile.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	checkUsed(t, repo, owner, 0)
}

func TestReadyDedupChargesOncePerOwner(t *testing.T) {
	repo := componentRepository(t)
	owner, other := uuid.NewString(), uuid.NewString()
	first := reserveAndComplete(t, repo, owner)
	charge := first.SizeBytes
	publishFixture(t, repo, claimForFile(t, repo, first.ID, "scan_and_process"), first, "originals/first")
	next := reserveAndComplete(t, repo, owner)
	checkUsed(t, repo, owner, 2*charge)
	publishDuplicate(t, repo, next)
	checkUsed(t, repo, owner, charge)
	distinctOwner := reserveAndComplete(t, repo, other)
	publishDuplicate(t, repo, distinctOwner)
	checkUsed(t, repo, other, charge)

	if err := repo.SoftDeleteFile(t.Context(), first.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	checkUsed(t, repo, owner, charge)
	if err := repo.SoftDeleteFile(t.Context(), next.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	checkUsed(t, repo, owner, 0)
	checkUsed(t, repo, other, charge)
}

func TestTerminalScanFailureReleasesReservation(t *testing.T) {
	repo := componentRepository(t)
	owner := uuid.NewString()
	file := reserveAndComplete(t, repo, owner)
	ctx := t.Context()
	if _, err := repo.pool.Exec(ctx, "UPDATE media_jobs SET attempts=9 WHERE file_id=$1", file.ID); err != nil {
		t.Fatal(err)
	}
	job := claimForFile(t, repo, file.ID, "scan_and_process")
	checkUsed(t, repo, owner, file.SizeBytes)
	if err := repo.RetryJob(ctx, job, errors.New("fixture processing failure")); err != nil {
		t.Fatal(err)
	}
	failed, err := repo.GetFile(ctx, file.ID)
	if err != nil || failed.Status != domain.StatusRejected || failed.FailureCode != apperror.ProcessingFailed {
		t.Error("scan failure did not reject the upload", failed.Status, failed.FailureCode, err)
	}
	checkUsed(t, repo, owner, 0)
}

func TestQuotaMigrationCountsExistingFiles(t *testing.T) {
	owner := uuid.NewString()
	pendingID, failedID := uuid.NewString(), uuid.NewString()
	ctx := t.Context()
	pool := testutil.PostgresWithSeed(t, "202611200004_media_quota.sql", func(pool *pgxpool.Pool) {
		_, err := pool.Exec(ctx, `
            WITH shared_blob AS (
                INSERT INTO blobs(id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key,scan_status)
                VALUES(gen_random_uuid(),'fixture',1024,'private','application/pdf','originals/legacy','clean')
                RETURNING id
            ), copied AS (
                INSERT INTO files(id,owner_user_id,blob_id,purpose,visibility,original_name,declared_content_type,size_bytes,checksum_sha256,status)
                SELECT gen_random_uuid(),$1,b.id,'attachment','private','fixture','application/pdf',1024,'fixture','ready'
                  FROM shared_blob b,generate_series(1,2)
            ), unfinished AS (
                INSERT INTO files(id,owner_user_id,purpose,visibility,original_name,declared_content_type,size_bytes,checksum_sha256,status)
                VALUES($2,$1,'attachment','private','fixture','application/pdf',256,'fixture','pending_upload'),
                      ($3,$1,'attachment','private','fixture','application/pdf',256,'fixture','processing')
                RETURNING id
            ), old_session AS (
                INSERT INTO upload_sessions(file_id,mode,bucket,object_key,expected_size,checksum_sha256,content_type,expires_at,multipart_id)
                SELECT id,'multipart','q','uploads/'||id::text,256,'fixture','application/pdf',NOW()+interval '1 hour','legacy-multipart'
                  FROM unfinished WHERE id=$2
            )
            INSERT INTO media_jobs(id,file_id,kind,status)
            SELECT gen_random_uuid(),id,'scan_and_process','failed' FROM unfinished WHERE id=$3`, owner, pendingID, failedID)
		if err != nil {
			t.Fatal(err)
		}
	})
	repo := New(pool, false)
	checkUsed(t, repo, owner, 1280)
	file, session := quotaUpload(owner, domain.UploadModeMultipart)
	requireQuotaError(t, repo.CreateUpload(ctx, file, session, domain.UploadQuota{MaxBytes: 4096, MaxMultipartSessions: 1}), apperror.UploadLimitReached)
	failed, err := repo.GetFile(ctx, failedID)
	if err != nil || failed.Status != domain.StatusRejected {
		t.Error("legacy failure still owns quota", failed.Status, err)
	}
	if _, err := repo.CompleteUpload(ctx, pendingID, func(context.Context, domain.UploadSession) (string, error) {
		return "application/pdf", nil
	}); err != nil {
		t.Fatal(err)
	}
	checkUsed(t, repo, owner, 1280)
	if err := repo.CreateUpload(ctx, file, session, domain.UploadQuota{MaxBytes: 4096, MaxMultipartSessions: 1}); err != nil {
		t.Error("completed multipart did not release its slot", err)
	}
}

func checkUsed(t *testing.T, repo *Postgres, owner string, want int64) {
	t.Helper()
	if used, err := repo.ActiveSize(t.Context(), owner); err != nil || used != want {
		t.Error("unexpected used bytes", used, "want", want, err)
	}
}

func quotaUpload(owner string, mode domain.UploadMode) (domain.File, domain.UploadSession) {
	file, session := uploadFixture()
	file.OwnerUserID = owner
	session.Mode = mode
	return file, session
}

func reserveAndComplete(t *testing.T, repo *Postgres, owner string) domain.File {
	t.Helper()
	file, session := quotaUpload(owner, domain.UploadModeSingle)
	if err := repo.CreateUpload(t.Context(), file, session, testutil.UploadQuota()); err != nil {
		t.Fatal(err)
	}
	file, err := repo.CompleteUpload(t.Context(), file.ID, func(context.Context, domain.UploadSession) (string, error) {
		return file.DeclaredContentType, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func publishDuplicate(t *testing.T, repo *Postgres, file domain.File) {
	t.Helper()
	job := claimForFile(t, repo, file.ID, "scan_and_process")
	ctx := t.Context()
	if err := repo.MarkProcessing(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, reused, err := repo.LinkCleanBlob(ctx, job, file, file.DeclaredContentType, "originals/"+file.ID, true); err != nil || !reused {
		t.Fatal("expected a deduplicated blob", reused, err)
	}
	if err := repo.SaveVariants(ctx, job, "", nil); err != nil {
		t.Fatal(err)
	}
}

func requireQuotaError(t *testing.T, err error, code string) {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Code != code {
		t.Error("expected", code, "got", err)
	}
}

func BenchmarkQuotaUsage(b *testing.B) {
	pool := testutil.Postgres(b)
	repo := New(pool, false)
	owner := uuid.NewString()
	if _, err := pool.Exec(b.Context(), `INSERT INTO files(id,owner_user_id,purpose,visibility,original_name,
        declared_content_type,size_bytes,checksum_sha256,status)
        SELECT gen_random_uuid(),$1,'attachment','private','fixture','application/pdf',1024,'fixture','ready'
        FROM generate_series(1,6000)`, owner); err != nil {
		b.Fatal(err)
	}
	b.Run("sum_files", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var size int64
			err := pool.QueryRow(b.Context(), `SELECT COALESCE(SUM(size_bytes),0) FROM files
                WHERE owner_user_id=$1 AND status NOT IN ('rejected','deleted')`, owner).Scan(&size)
			if err != nil || size != 6000*1024 {
				b.Fatal(size, err)
			}
		}
	})
	b.Run("owner_counter", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if size, err := repo.ActiveSize(b.Context(), owner); err != nil || size != 6000*1024 {
				b.Fatal(size, err)
			}
		}
	})
}
