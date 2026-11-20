package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/domain"
)

const JobLease = 2 * time.Minute

var ErrLeaseLost = errors.New("media job lease lost")
var ErrScanCancelled = errors.New("media file no longer needs processing")

const claimJobSQL = `
    WITH pending AS (
        SELECT id, available_at AS due_at FROM media_jobs
         WHERE status='pending' AND available_at <= statement_timestamp()
           AND ($2 OR kind<>'cleanup_upload')
         ORDER BY available_at,created_at LIMIT 1 FOR UPDATE SKIP LOCKED
    ), expired AS (
        SELECT id, lease_until AS due_at FROM media_jobs
         WHERE status='running' AND lease_until <= statement_timestamp()
           AND ($2 OR kind<>'cleanup_upload')
         ORDER BY lease_until,id LIMIT 1 FOR UPDATE SKIP LOCKED
    ), candidates AS (
        SELECT id,due_at FROM pending UNION ALL SELECT id,due_at FROM expired
    ), candidate AS (
        SELECT id FROM candidates ORDER BY due_at,id LIMIT 1
    )
    UPDATE media_jobs j
       SET status='running', locked_at=statement_timestamp(), updated_at=statement_timestamp(),
           attempts=j.attempts+1, lease_token=gen_random_uuid(),
           lease_until=statement_timestamp()+make_interval(secs => $1)
      FROM candidate c WHERE j.id=c.id
    RETURNING j.id,j.file_id,j.kind,j.attempts,j.lease_token`

func (r *Postgres) ClaimJob(ctx context.Context, allowCleanup bool) (domain.Job, bool, error) {
	var job domain.Job
	err := r.pool.QueryRow(ctx, claimJobSQL, JobLease.Seconds(), allowCleanup).
		Scan(&job.ID, &job.FileID, &job.Kind, &job.Attempts, &job.LeaseToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, false, nil
	}
	if err != nil {
		return domain.Job{}, false, fmt.Errorf("claim media job: %w", err)
	}
	return job, true, nil
}

func (r *Postgres) RenewJob(ctx context.Context, job domain.Job) error {
	tag, err := r.pool.Exec(ctx, `
        UPDATE media_jobs
           SET lease_until=statement_timestamp()+make_interval(secs => $3)
         WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_until>statement_timestamp()`,
		job.ID, job.LeaseToken, JobLease.Seconds())
	if err != nil {
		return fmt.Errorf("renew media job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func lockJob(ctx context.Context, tx pgx.Tx, job domain.Job) error {
	var locked int
	err := tx.QueryRow(ctx, `
        SELECT 1 FROM media_jobs WHERE id=$1 AND lease_token=$2 AND file_id=$3 AND kind=$4
         AND status='running' AND lease_until>statement_timestamp() FOR UPDATE`,
		job.ID, job.LeaseToken, job.FileID, job.Kind).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func finishJob(ctx context.Context, tx pgx.Tx, job domain.Job) error {
	tag, err := tx.Exec(ctx, `UPDATE media_jobs SET status='completed',updated_at=statement_timestamp(),
        locked_at=NULL,lease_token=NULL,lease_until=NULL
        WHERE id=$1 AND status='running' AND lease_token=$2 AND lease_until>statement_timestamp()`, job.ID, job.LeaseToken)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return err
}

func (r *Postgres) CompleteJob(ctx context.Context, job domain.Job) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := lockJob(ctx, tx, job); err != nil {
			return err
		}
		return finishJob(ctx, tx, job)
	})
}

func (r *Postgres) TrackObject(ctx context.Context, job domain.Job, key string) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := lockJob(ctx, tx, job); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO media_job_objects(object_key,job_id,lease_token)
            VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, key, job.ID, job.LeaseToken)
		return err
	})
}

func untrackObject(ctx context.Context, tx pgx.Tx, job domain.Job, key string) error {
	_, err := tx.Exec(ctx, `DELETE FROM media_job_objects WHERE object_key=$1 AND job_id=$2 AND lease_token=$3`,
		key, job.ID, job.LeaseToken)
	return err
}

func (r *Postgres) ForgetStagedObject(ctx context.Context, job domain.Job, key string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM media_job_objects WHERE object_key=$1 AND job_id=$2 AND lease_token=$3`,
		key, job.ID, job.LeaseToken)
	return err
}
