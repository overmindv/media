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

const zipContentType = "application/zip"

type variantRenderer interface {
	Render(context.Context, string, []int, func(int, io.Reader, int64) error) error
}

type Processor struct {
	repository        *repository.Postgres
	storage           service.ObjectStorage
	scanner           *ClamAV
	renderer          variantRenderer
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
	if markErr := p.repository.MarkProcessing(ctx, job); markErr != nil {
		return markErr
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
	if trackErr := p.repository.TrackObject(ctx, job, objectKey); trackErr != nil {
		return trackErr
	}
	// Presigned POST still allows writes to the quarantine key.
	original, err := os.Open(temporaryPath) //nolint:gosec // worker-owned temp file
	if err != nil {
		return fmt.Errorf("open scanned snapshot: %w", err)
	}
	putErr := p.storage.Put(ctx, p.config.S3.MediaBucket, objectKey, detectedType, original, file.SizeBytes)
	if snapshotErr := errors.Join(putErr, original.Close()); snapshotErr != nil {
		return fmt.Errorf("сохранить проверенную копию: %w", snapshotErr)
	}
	blob, deduplicated, err := p.repository.LinkCleanBlob(ctx, job, file, detectedType, objectKey, verified)
	if err != nil {
		return err
	}
	if deduplicated {
		if deleteErr := p.storage.Delete(ctx, p.config.S3.MediaBucket, objectKey); deleteErr != nil {
			return fmt.Errorf("удалить deduplicated object: %w", deleteErr)
		}
		if forgetErr := p.repository.ForgetStagedObject(ctx, job, objectKey); forgetErr != nil {
			return forgetErr
		}
	}
	variants := make([]domain.Variant, 0)
	var prefix string
	if imageConfig != nil && file.Purpose.IsImage() {
		prefix = "variants/private/"
		if verified && file.Visibility == domain.VisibilityPublic {
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
	defer func() { _ = os.RemoveAll(dir) }()
	err = p.renderer.Render(ctx, inputPath, widths, func(width int, body io.Reader, size int64) error {
		part, openErr := os.OpenFile(filepath.Join(dir, strconv.Itoa(width)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // width comes from the fixed size list
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.CopyN(part, body, size)

		return errors.Join(copyErr, part.Close())
	})
	if err != nil {
		return nil, err
	}
	result := make([]domain.Variant, 0, len(widths))
	previews := make([]*os.File, 0, len(widths))
	defer func() {
		for _, preview := range previews {
			_ = preview.Close()
		}
	}()
	for _, width := range widths {
		name := "w" + strconv.Itoa(width)
		path := filepath.Join(dir, strconv.Itoa(width))
		key := prefix + name + ".webp"
		variant, preview, err := openVariant(path, blobID, name, key, width)
		if err != nil {
			return nil, err
		}
		result = append(result, variant)
		previews = append(previews, preview)
	}
	for i, variant := range result {
		if track != nil {
			if err := track(variant.ObjectKey); err != nil {
				return nil, err
			}
		}
		if err := p.storeVariant(ctx, variant, previews[i]); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func openVariant(path, blobID, name, key string, maxWidth int) (domain.Variant, *os.File, error) {
	file, err := os.Open(path) //nolint:gosec // worker-owned rendered variant
	if err != nil {
		return domain.Variant{}, nil, fmt.Errorf("открыть WebP variant: %w", err)
	}
	variant, err := readVariant(file, blobID, name, key, maxWidth)
	if err != nil {
		_ = file.Close()

		return domain.Variant{}, nil, err
	}

	return variant, file, nil
}

func readVariant(file *os.File, blobID, name, key string, maxWidth int) (domain.Variant, error) {
	info, err := file.Stat()
	if err != nil {
		return domain.Variant{}, fmt.Errorf("прочитать WebP stat: %w", err)
	}
	dimensions, err := webp.DecodeConfig(file)
	if err != nil {
		return domain.Variant{}, fmt.Errorf("read rendered WebP dimensions: %w", err)
	}
	if dimensions.Width > maxWidth || render.CheckDimensions(dimensions, render.MaxPixels) != nil {
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

	return domain.Variant{ID: uuid.NewString(), BlobID: blobID, Name: name, Format: "webp", Width: dimensions.Width, Height: dimensions.Height, SizeBytes: info.Size(), ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)), ObjectKey: key, CreatedAt: time.Now().UTC()}, nil
}

func (p *Processor) storeVariant(ctx context.Context, variant domain.Variant, reader io.Reader) error {
	if err := p.storage.Put(ctx, p.config.S3.MediaBucket, variant.ObjectKey, "image/webp", reader, variant.SizeBytes); err != nil {
		return fmt.Errorf("загрузить WebP variant: %w", err)
	}

	return nil
}

func inspectFile(path, declaredType string, maxPixels int64) (string, *image.Config, error) {
	file, err := os.Open(path) //nolint:gosec // путь исходника создан worker во временном каталоге
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
	if detected == "application/pdf" || detected == zipContentType || detected == "application/gzip" || detected == "application/x-tar" {
		if detected == zipContentType && strings.HasPrefix(declaredType, "application/vnd.openxmlformats-officedocument") {
			return declaredType, nil, nil
		}

		return detected, nil, nil
	}

	return "", nil, fmt.Errorf("тип файла не разрешён")
}
