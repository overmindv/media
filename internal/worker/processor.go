package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/render"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/service"
	"golang.org/x/image/webp"
)

type Processor struct {
	repository        *repository.Postgres
	storage           service.ObjectStorage
	scanner           *ClamAV
	renderer          *render.Client
	config            config.Config
	logger            *slog.Logger
	nextOrphanCleanup time.Time
	nextUploadCleanup time.Time
}

// NewProcessor создаёт worker обработки quarantine-файлов.
func NewProcessor(repository *repository.Postgres, storage service.ObjectStorage, scanner *ClamAV, cfg config.Config, logger *slog.Logger) *Processor {
	return &Processor{
		repository: repository,
		storage:    storage,
		scanner:    scanner,
		renderer:   render.NewClient(cfg.RenderSocket),
		config:     cfg,
		logger:     logger,
	}
}

// Run опрашивает PostgreSQL queue до отмены context.
func (p *Processor) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.config.WorkerPoll)
	defer ticker.Stop()
	for {
		if err := p.runOne(ctx); err != nil {
			p.logger.Error("media worker iteration failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runOne выполняет периодическую очистку orphan avatars и обрабатывает одно задание очереди.
func (p *Processor) runOne(ctx context.Context) error {
	if time.Now().After(p.nextOrphanCleanup) {
		if err := p.repository.ScheduleOrphanAvatars(ctx, 24*time.Hour, p.config.DeleteRetention); err != nil {
			return fmt.Errorf("запланировать orphan avatar cleanup: %w", err)
		}
		p.nextOrphanCleanup = time.Now().Add(time.Hour)
	}
	if p.config.EnableUploadCleanup && time.Now().After(p.nextUploadCleanup) {
		p.nextUploadCleanup = time.Now().Add(time.Minute)
		cleanup, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := p.sweep(cleanup, time.Now()); err != nil {
			p.logger.WarnContext(ctx, "media cleanup failed", "error", err)
		}
		cancel()
	}
	job, found, err := p.repository.ClaimJob(ctx, p.config.EnableUploadCleanup)
	if err != nil {
		return fmt.Errorf("получить media job: %w", err)
	}
	if !found {
		return nil
	}
	jobCtx, release := p.keepLease(ctx, job)
	defer release()
	switch job.Kind {
	case "scan_and_process":
		err = p.process(jobCtx, job)
	case "purge":
		err = p.purge(jobCtx, job)
	case "cleanup_upload":
		err = p.cleanupUpload(jobCtx, job)
	default:
		err = fmt.Errorf("неподдерживаемый job kind %s", job.Kind)
	}
	if err != nil {
		retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		var retryErr error
		switch {
		case errors.Is(err, repository.ErrLeaseLost), errors.Is(context.Cause(jobCtx), repository.ErrLeaseLost):
			return repository.ErrLeaseLost
		case errors.Is(err, repository.ErrScanCancelled):
			return p.repository.CompleteJob(retryCtx, job)
		case errors.Is(err, errScanUnavailable):
			retryErr = p.repository.RetryScanJob(retryCtx, job)
		default:
			retryErr = p.repository.RetryJob(retryCtx, job, err)
		}
		if retryErr != nil {
			return fmt.Errorf("обработка и постановка retry: %w", errors.Join(err, retryErr))
		}
		return err
	}

	return nil
}

func (p *Processor) keepLease(ctx context.Context, job domain.Job) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(repository.JobLease / 4)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				request, stop := context.WithTimeout(ctx, 3*time.Second)
				err := p.repository.RenewJob(request, job)
				stop()
				if err != nil {
					cancel(errors.Join(repository.ErrLeaseLost, err))
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel(nil)
		<-done
	}
}

func (p *Processor) purge(ctx context.Context, job domain.Job) error {
	keys, err := p.repository.PreparePurge(ctx, job)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := p.storage.Delete(ctx, p.config.S3.MediaBucket, key); err != nil {
			return fmt.Errorf("удалить physical media object: %w", err)
		}
	}

	return p.repository.CompleteJob(ctx, job)
}

func (p *Processor) process(ctx context.Context, job domain.Job) error {
	file, err := p.repository.GetFile(ctx, job.FileID)
	if err != nil {
		return fmt.Errorf("получить файл job: %w", err)
	}
	if file.Status != domain.StatusQuarantined && file.Status != domain.StatusProcessing {
		return repository.ErrScanCancelled
	}
	session, err := p.repository.GetUploadSession(ctx, job.FileID)
	if err != nil {
		return fmt.Errorf("получить upload session job: %w", err)
	}
	temporary, err := os.CreateTemp("", "overmindv-media-*")
	if err != nil {
		return fmt.Errorf("создать временный файл: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	scanCtx, cancelScan := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelScan()
	body, err := p.storage.Get(scanCtx, session.Bucket, session.ObjectKey)
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("читать quarantine object: %w", err)
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: body, N: file.SizeBytes + 1}
	input := io.TeeReader(limited, io.MultiWriter(temporary, hash))
	verified := !p.config.AllowUnscannedLocal
	clean := true
	var scanErr error
	if verified {
		clean, scanErr = p.scanner.Scan(scanCtx, input)
	} else {
		_, scanErr = io.Copy(io.Discard, input)
	}
	closeBodyErr := body.Close()
	closeTempErr := temporary.Close()
	cancelScan()
	if scanErr != nil || closeBodyErr != nil || closeTempErr != nil {
		return fmt.Errorf("read and scan quarantine object: %w", errors.Join(scanErr, closeBodyErr, closeTempErr))
	}
	if limited.N != 1 || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), file.ChecksumSHA256) {
		return p.reject(ctx, job, session, apperror.UploadIntegrityFailed)
	}
	if !clean {
		return p.reject(ctx, job, session, apperror.FileInfected)
	}
	if err := p.repository.MarkProcessing(ctx, job); err != nil {
		return err
	}
	maxPixels := p.config.Limits.MaxImagePixels
	if file.Purpose == domain.PurposeAvatar {
		maxPixels = p.config.Limits.MaxAvatarPixels
	}
	detectedType, imageConfig, err := inspectFile(temporaryPath, file.DeclaredContentType, maxPixels)
	if err != nil {
		if errors.Is(err, render.ErrTooLarge) {
			return p.reject(ctx, job, session, apperror.FileTooLarge)
		}

		return p.reject(ctx, job, session, apperror.UnsupportedMediaType)
	}
	if file.Purpose.IsImage() && imageConfig == nil {
		return p.reject(ctx, job, session, apperror.UnsupportedMediaType)
	}
	objectKey := "originals/verified/" + file.ID
	if !verified {
		objectKey = "originals/local/" + file.ID
	}
	objectKey += "/" + job.LeaseToken
	if err := p.repository.TrackObject(ctx, job, objectKey); err != nil {
		return err
	}
	// Publish the scanned snapshot, not another read of the upload key.
	original, err := os.Open(temporaryPath)
	if err != nil {
		return fmt.Errorf("open scanned snapshot: %w", err)
	}
	putErr := p.storage.Put(ctx, p.config.S3.MediaBucket, objectKey, detectedType, original, file.SizeBytes)
	if err := errors.Join(putErr, original.Close()); err != nil {
		return fmt.Errorf("store scanned snapshot: %w", err)
	}
	blob, deduplicated, err := p.repository.LinkCleanBlob(ctx, job, file, detectedType, objectKey, verified)
	if err != nil {
		return err
	}
	if deduplicated {
		if err := p.storage.Delete(ctx, p.config.S3.MediaBucket, objectKey); err != nil {
			return fmt.Errorf("удалить deduplicated object: %w", err)
		}
		if err := p.repository.ForgetStagedObject(ctx, job, objectKey); err != nil {
			return err
		}
	}
	variants := make([]domain.Variant, 0)
	var prefix string
	if imageConfig != nil && file.Purpose.IsImage() {
		prefix = "variants/private/"
		if file.Visibility == domain.VisibilityPublic {
			prefix = domain.PublicVariantPrefix
		}
		prefix += blob.ID + "/"
		var existing map[string]bool
		if deduplicated {
			existing, err = p.repository.VariantNames(ctx, blob.ID, prefix)
			if err != nil {
				return fmt.Errorf("get existing variants: %w", err)
			}
		}
		variants, err = p.createVariants(ctx, temporaryPath, blob.ID, prefix+job.LeaseToken+"/", imageConfig.Width, existing,
			func(key string) error { return p.repository.TrackObject(ctx, job, key) })
		if err != nil {
			if errors.Is(err, render.ErrTooLarge) {
				return p.reject(ctx, job, session, apperror.FileTooLarge)
			}
			if errors.Is(err, render.ErrRejected) {
				return p.reject(ctx, job, session, apperror.UnsupportedMediaType)
			}
			return err
		}
	}
	if err := p.repository.SaveVariants(ctx, job, prefix, variants); err != nil {
		return err
	}
	if err := p.storage.Delete(ctx, session.Bucket, session.ObjectKey); err != nil {
		p.logger.Warn("не удалось удалить quarantine object", "file_id", file.ID, "error", err)
	}

	return nil
}

func (p *Processor) reject(ctx context.Context, job domain.Job, session domain.UploadSession, code string) error {
	if err := p.repository.RejectFile(ctx, job, code); err != nil {
		return err
	}
	if err := p.storage.Delete(ctx, session.Bucket, session.ObjectKey); err != nil {
		p.logger.Warn("не удалось удалить rejected object", "file_id", job.FileID, "error", err)
	}

	return nil
}

func (p *Processor) createVariants(ctx context.Context, inputPath, blobID, prefix string, inputWidth int, existing map[string]bool, track func(string) error) ([]domain.Variant, error) {
	widths := make([]int, 0, 4)
	for _, width := range [...]int{128, 320, 768, 1440} {
		if !existing["w"+strconv.Itoa(width)] {
			widths = append(widths, width)
		}
		if width >= inputWidth {
			break
		}
	}
	if len(widths) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "media-webp-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	err = p.renderer.Render(ctx, inputPath, widths, func(width int, body io.Reader, size int64) error {
		part, err := os.OpenFile(filepath.Join(dir, strconv.Itoa(width)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.CopyN(part, body, size)
		return errors.Join(err, part.Close())
	})
	if err != nil {
		return nil, err
	}
	result := make([]domain.Variant, 0, len(widths))
	for _, width := range widths {
		name := "w" + strconv.Itoa(width)
		path := filepath.Join(dir, strconv.Itoa(width))
		key := prefix + name + ".webp"
		if track != nil {
			if err := track(key); err != nil {
				return nil, err
			}
		}
		variant, err := p.uploadVariant(ctx, path, blobID, name, key)
		if err != nil {
			return nil, err
		}
		result = append(result, variant)
	}
	return result, nil
}

func (p *Processor) uploadVariant(ctx context.Context, path, blobID, name, key string) (domain.Variant, error) {
	file, err := os.Open(path)
	if err != nil {
		return domain.Variant{}, fmt.Errorf("открыть WebP variant: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return domain.Variant{}, fmt.Errorf("прочитать WebP stat: %w", err)
	}
	dimensions, err := webp.DecodeConfig(file)
	if err != nil {
		return domain.Variant{}, fmt.Errorf("read rendered WebP dimensions: %w", err)
	}
	if err := render.CheckDimensions(dimensions, render.MaxPixels); err != nil {
		return domain.Variant{}, fmt.Errorf("invalid renderer output")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return domain.Variant{}, fmt.Errorf("rewind rendered WebP: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return domain.Variant{}, fmt.Errorf("вычислить checksum variant: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return domain.Variant{}, fmt.Errorf("перемотать variant: %w", err)
	}
	if err := p.storage.Put(ctx, p.config.S3.MediaBucket, key, "image/webp", file, info.Size()); err != nil {
		return domain.Variant{}, fmt.Errorf("загрузить WebP variant: %w", err)
	}
	return domain.Variant{ID: uuid.NewString(), BlobID: blobID, Name: name, Format: "webp", Width: dimensions.Width, Height: dimensions.Height, SizeBytes: info.Size(), ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)), ObjectKey: key, CreatedAt: time.Now().UTC()}, nil
}

func inspectFile(path, declaredType string, maxPixels int64) (string, *image.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, fmt.Errorf("открыть файл для inspect: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	header := make([]byte, 512)
	count, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", nil, fmt.Errorf("прочитать magic bytes: %w", err)
	}
	detected := http.DetectContentType(header[:count])
	if detected == "image/jpeg" || detected == "image/png" || detected == "image/webp" {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return "", nil, fmt.Errorf("перемотать изображение: %w", err)
		}
		config, _, err := image.DecodeConfig(file)
		if err != nil {
			return "", nil, fmt.Errorf("декодировать заголовок изображения: %w", err)
		}
		if err := render.CheckDimensions(config, maxPixels); err != nil {
			return "", nil, err
		}
		return detected, &config, nil
	}
	if detected == "application/pdf" || detected == "application/zip" || detected == "application/gzip" || detected == "application/x-tar" {
		if detected == "application/zip" && strings.HasPrefix(declaredType, "application/vnd.openxmlformats-officedocument") {
			return declaredType, nil, nil
		}
		return detected, nil, nil
	}

	return "", nil, fmt.Errorf("тип файла не разрешён")
}
