package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/overmindv/media/internal/domain"
)

func (p *Processor) cleanupUpload(ctx context.Context, job domain.Job) error {
	ctx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	session, err := p.repository.PrepareUploadCleanup(ctx, job)
	if err != nil {
		return err
	}
	if session.FileID == "" {
		return p.repository.CompleteJob(ctx, job)
	}
	if session.Bucket != p.config.S3.QuarantineBucket || !strings.HasPrefix(session.ObjectKey, "uploads/") {
		return errors.New("unexpected upload cleanup target")
	}
	if session.Mode == domain.UploadModeMultipart && session.CompletedAt == nil && session.MultipartID != "" {
		if err := p.storage.AbortMultipart(ctx, session.Bucket, session.ObjectKey, session.MultipartID); err != nil {
			return err
		}
	}
	if err := p.storage.Delete(ctx, session.Bucket, session.ObjectKey); err != nil {
		return err
	}
	return p.repository.CompleteUploadCleanup(ctx, job)
}

func (p *Processor) sweep(ctx context.Context, now time.Time) error {
	// Old presigned part URLs could outlive the session by as much as UploadTTL.
	cutoff := now.Add(-p.config.UploadTTL).Add(-5 * time.Minute)
	if err := p.repository.ScheduleUploadCleanup(ctx, cutoff); err != nil {
		return fmt.Errorf("schedule upload cleanup: %w", err)
	}
	keys, err := p.repository.StagedCleanupTargets(ctx, now.Add(-time.Hour))
	if err != nil {
		return fmt.Errorf("list staged cleanup targets: %w", err)
	}
	var failures error
	for _, key := range keys {
		if err := p.deleteMediaObject(ctx, key); err != nil {
			failures = errors.Join(failures, err)
		} else {
			failures = errors.Join(failures, p.repository.CompleteStagedCleanup(ctx, key))
		}
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
	}
	blobs, err := p.repository.UnboundCleanupTargets(ctx, now.Add(-p.config.DeleteRetention))
	if err != nil {
		return errors.Join(failures, fmt.Errorf("list unbound blobs: %w", err))
	}
	for _, blob := range blobs {
		var failed error
		for _, key := range blob.Keys {
			if err := p.deleteMediaObject(ctx, key); err != nil {
				failed = err
				break
			}
		}
		if failed == nil {
			failed = p.repository.CompleteUnboundCleanup(ctx, blob.ID)
		}
		failures = errors.Join(failures, failed)
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
	}
	return failures
}

func (p *Processor) deleteMediaObject(ctx context.Context, key string) error {
	if !strings.HasPrefix(key, "originals/") && !strings.HasPrefix(key, "variants/") {
		return errors.New("unexpected media cleanup target")
	}
	return p.storage.Delete(ctx, p.config.S3.MediaBucket, key)
}
