//go:build component

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/testutil"
)

func TestJobLeaseReclaimFencesOldWorker(t *testing.T) {
	repo := componentRepository(t)
	second := New(repo.pool, false)
	file := completeFixture(t, repo)
	type claim struct {
		job   domain.Job
		found bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan claim, 2)
	for _, worker := range []*Postgres{repo, second} {
		go func() {
			<-start
			job, found, err := worker.ClaimJob(t.Context(), false)
			results <- claim{job, found, err}
		}()
	}
	close(start)
	var old domain.Job
	for range 2 {
		got := <-results
		if got.err != nil || got.found && old.ID != "" {
			t.Fatal("duplicate or failed concurrent claim", got.err)
		}
		if got.found {
			old = got.job
		}
	}
	if old.ID == "" || old.FileID != file.ID {
		t.Fatal("queued job was not claimed")
	}
	if err := repo.RenewJob(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := repo.TrackObject(t.Context(), old, "originals/interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := second.ClaimJob(t.Context(), false); err != nil || found {
		t.Fatal("second worker claimed a live lease", found, err)
	}
	if _, err := repo.pool.Exec(t.Context(), "UPDATE media_jobs SET lease_until=NOW()-interval '1 second' WHERE id=$1", old.ID); err != nil {
		t.Fatal(err)
	}
	current := claimForFile(t, second, file.ID, "scan_and_process")
	if old.ID != current.ID || old.LeaseToken == current.LeaseToken || current.Attempts != 2 {
		t.Fatal("reclaim did not create a new lease", old.ID, current.ID, current.Attempts)
	}
	for _, apply := range []func() error{
		func() error { return repo.RenewJob(t.Context(), old) },
		func() error { return repo.CompleteJob(t.Context(), old) },
		func() error { return repo.RejectFile(t.Context(), old, "stale") },
		func() error { return repo.MarkProcessing(t.Context(), old) },
		func() error { return repo.RetryScanJob(t.Context(), old) },
		func() error { return repo.SaveVariants(t.Context(), old, "", nil) },
		func() error { return repo.TrackObject(t.Context(), old, "originals/stale") },
	} {
		if err := apply(); !errors.Is(err, ErrLeaseLost) {
			t.Error("outdated lease was not rejected", err)
		}
	}
	publishFixture(t, second, current, file, "originals/current")
	var publications int
	if err := repo.pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox_events WHERE event_type='file.ready' AND aggregate_id=$1", file.ID).
		Scan(&publications); err != nil || publications != 1 {
		t.Error("ready event must be written once", publications, err)
	}
	var staged []string
	if err := repo.pool.QueryRow(t.Context(), "SELECT ARRAY(SELECT object_key FROM media_job_objects WHERE job_id=$1)", old.ID).
		Scan(&staged); err != nil || len(staged) != 1 || staged[0] != "originals/interrupted" {
		t.Error("only the interrupted upload must remain tracked", staged, err)
	}
}

func TestPurgeTombstonePreventsReuse(t *testing.T) {
	repo := componentRepository(t)
	first := completeFixture(t, repo)
	publishFixture(t, repo, claimForFile(t, repo, first.ID, "scan_and_process"), first, "originals/retired")
	if err := repo.SoftDeleteFile(t.Context(), first.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	purgeJob := claimForFile(t, repo, first.ID, "purge")
	keys, err := repo.PreparePurge(t.Context(), purgeJob)
	if err != nil || len(keys) != 1 || keys[0] != "originals/retired" {
		t.Fatal("expected old object as purge target", keys, err)
	}
	second := completeFixture(t, repo)
	scan := claimForFile(t, repo, second.ID, "scan_and_process")
	publishFixture(t, repo, scan, second, "originals/replacement")
	if _, err := repo.pool.Exec(t.Context(), "UPDATE media_jobs SET lease_until=NOW()-interval '1 second' WHERE id=$1", purgeJob.ID); err != nil {
		t.Fatal(err)
	}
	replay := claimForFile(t, repo, first.ID, "purge")
	keys, err = repo.PreparePurge(t.Context(), replay)
	if err != nil || len(keys) != 1 || keys[0] != "originals/retired" {
		t.Error("retry must only see the retired generation", keys, err)
	}
	if err := repo.CompleteJob(t.Context(), purgeJob); !errors.Is(err, ErrLeaseLost) {
		t.Error("expired purge owner completed a newer lease", err)
	}
	if key, err := repo.GetObjectKey(t.Context(), second.ID, "original"); err != nil || key != "originals/replacement" {
		t.Error("replacement was affected by purge", key, err)
	}
}

func TestSoftDeleteWaitsForBinding(t *testing.T) {
	repo := componentRepository(t)
	file := completeFixture(t, repo)
	publishFixture(t, repo, claimForFile(t, repo, file.ID, "scan_and_process"), file, "originals/bound")
	ctx := t.Context()
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM files WHERE id=$1 FOR UPDATE", file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO file_bindings(id,file_id,service_name,entity_type,entity_id)
        VALUES($1,$2,'users','user_avatar',$3)`, uuid.NewString(), file.ID, file.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := repo.SoftDeleteFile(blocked, file.ID, time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Error("deletion did not wait for the binding transaction", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = repo.SoftDeleteFile(ctx, file.ID, time.Now())
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Code != apperror.FileInUse {
		t.Error("bound file was not protected", err)
	}
}

func completeFixture(t *testing.T, repo *Postgres) domain.File {
	t.Helper()
	file, session := uploadFixture()
	if err := repo.CreateUpload(t.Context(), file, session); err != nil {
		t.Fatal(err)
	}
	ready, err := repo.CompleteUpload(t.Context(), file.ID, func(context.Context, domain.UploadSession) (string, error) {
		return file.DeclaredContentType, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func claimForFile(t *testing.T, repo *Postgres, fileID, kind string) domain.Job {
	t.Helper()
	job, found, err := repo.ClaimJob(t.Context(), false)
	if err != nil || !found || job.FileID != fileID || job.Kind != kind {
		t.Fatal("unexpected claimed job", job.ID, job.Kind, found, err)
	}
	return job
}

func publishFixture(t *testing.T, repo *Postgres, job domain.Job, file domain.File, key string) {
	t.Helper()
	ctx := t.Context()
	if err := repo.MarkProcessing(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := repo.TrackObject(ctx, job, key); err != nil {
		t.Fatal(err)
	}
	if _, reused, err := repo.LinkCleanBlob(ctx, job, file, file.DeclaredContentType, key, true); err != nil || reused {
		t.Fatal("unexpected blob link", reused, err)
	}
	if err := repo.SaveVariants(ctx, job, "", nil); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkClaimJobIdle(b *testing.B) {
	pool := testutil.Postgres(b)
	_, err := pool.Exec(b.Context(), `
        WITH new_file AS (
            INSERT INTO files (id,owner_user_id,purpose,visibility,original_name,declared_content_type,size_bytes,checksum_sha256,status)
            VALUES(gen_random_uuid(),gen_random_uuid(),'attachment','private','fixture','application/pdf',1,'fixture','pending_upload')
            RETURNING id
        )
        INSERT INTO media_jobs(id,file_id,kind,status)
        SELECT gen_random_uuid(),id,'scan_and_process','completed' FROM new_file,generate_series(1,10000)`)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("previous", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var unused string
			err := pool.QueryRow(b.Context(), `
                WITH candidate AS (
                    SELECT id FROM media_jobs WHERE status='pending' AND available_at <= NOW()
                     ORDER BY available_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1
                )
                UPDATE media_jobs j SET status='running',locked_at=NOW(),attempts=attempts+1,updated_at=NOW()
                 FROM candidate c WHERE j.id=c.id RETURNING j.id`).Scan(&unused)
			if !errors.Is(err, pgx.ErrNoRows) {
				b.Fatal("expected no work", err)
			}
		}
	})
	b.Run("lease", func(b *testing.B) {
		b.ReportAllocs()
		repo := New(pool, false)
		for b.Loop() {
			if _, found, err := repo.ClaimJob(b.Context(), false); err != nil || found {
				b.Fatal("expected no work", found, err)
			}
		}
	})
}
