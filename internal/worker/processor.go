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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/service"
	_ "golang.org/x/image/webp"
)

var errImagePixelLimit = errors.New("image pixel limit exceeded")

type Processor struct {
	repository        *repository.Postgres
	storage           service.ObjectStorage
	scanner           *ClamAV
	config            config.Config
	logger            *slog.Logger
	nextOrphanCleanup time.Time
}

// NewProcessor создаёт worker обработки quarantine-файлов.
func NewProcessor(repository *repository.Postgres, storage service.ObjectStorage, scanner *ClamAV, cfg config.Config, logger *slog.Logger) *Processor {
	return &Processor{
		repository: repository,
		storage:    storage,
		scanner:    scanner,
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
	job, found, err := p.repository.ClaimJob(ctx)
	if err != nil {
		return fmt.Errorf("получить media job: %w", err)
	}
	if !found {
		return nil
	}
	if job.Kind != "scan_and_process" {
		if job.Kind == "purge" {
			if err := p.purge(ctx, job); err != nil {
				_ = p.repository.RetryJob(ctx, job, err)
				return err
			}

			return nil
		}
		err = fmt.Errorf("неподдерживаемый job kind %s", job.Kind)
		_ = p.repository.RetryJob(ctx, job, err)
		return err
	}
	if err := p.process(ctx, job); err != nil {
		if retryErr := p.repository.RetryJob(ctx, job, err); retryErr != nil {
			return fmt.Errorf("обработка и постановка retry: %w", errors.Join(err, retryErr))
		}
		return err
	}

	return nil
}

func (p *Processor) purge(ctx context.Context, job domain.Job) error {
	keys, err := p.repository.PurgeTargets(ctx, job.FileID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := p.storage.Delete(ctx, p.config.S3.MediaBucket, key); err != nil {
			return fmt.Errorf("удалить physical media object: %w", err)
		}
	}

	return p.repository.CompletePurge(ctx, job)
}

func (p *Processor) process(ctx context.Context, job domain.Job) error {
	file, err := p.repository.GetFile(ctx, job.FileID)
	if err != nil {
		return fmt.Errorf("получить файл job: %w", err)
	}
	session, err := p.repository.GetUploadSession(ctx, job.FileID)
	if err != nil {
		return fmt.Errorf("получить upload session job: %w", err)
	}
	if err := p.repository.MarkProcessing(ctx, file.ID); err != nil {
		return err
	}
	temporary, err := os.CreateTemp("", "overmindv-media-*")
	if err != nil {
		return fmt.Errorf("создать временный файл: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	body, err := p.storage.Get(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("читать quarantine object: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(body, file.SizeBytes+1))
	closeBodyErr := body.Close()
	closeTempErr := temporary.Close()
	if copyErr != nil || closeBodyErr != nil || closeTempErr != nil {
		return fmt.Errorf("скопировать и закрыть quarantine object: %w", errors.Join(copyErr, closeBodyErr, closeTempErr))
	}
	if written != file.SizeBytes || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), file.ChecksumSHA256) {
		return p.reject(ctx, job, session, apperror.UploadIntegrityFailed)
	}
	if p.scanner.Available(2 * time.Second) {
		clean, err := p.scanner.ScanFile(temporaryPath)
		if err != nil {
			return fmt.Errorf("ClamAV scan: %w", err)
		}
		if !clean {
			return p.reject(ctx, job, session, apperror.FileInfected)
		}
	} else {
		// Антивирус необязателен: если clamd недоступен, деградируем до проверки без сканирования.
		p.logger.WarnContext(ctx, "ClamAV недоступен — антивирусная проверка пропущена",
			"file_id", file.ID, "purpose", file.Purpose)
	}
	maxPixels := p.config.Limits.MaxImagePixels
	if file.Purpose == domain.PurposeAvatar {
		maxPixels = p.config.Limits.MaxAvatarPixels
	}
	detectedType, imageConfig, err := inspectFile(temporaryPath, file.DeclaredContentType, maxPixels)
	if err != nil {
		if errors.Is(err, errImagePixelLimit) {
			return p.reject(ctx, job, session, apperror.FileTooLarge)
		}

		return p.reject(ctx, job, session, apperror.UnsupportedMediaType)
	}
	objectKey := "originals/" + file.ID
	if err := p.storage.Copy(ctx, session.Bucket, session.ObjectKey, p.config.S3.MediaBucket, objectKey, detectedType); err != nil {
		return fmt.Errorf("перенести clean original: %w", err)
	}
	blob, deduplicated, err := p.repository.LinkCleanBlob(ctx, file, detectedType, objectKey)
	if err != nil {
		return err
	}
	if deduplicated {
		if err := p.storage.Delete(ctx, p.config.S3.MediaBucket, objectKey); err != nil {
			return fmt.Errorf("удалить deduplicated object: %w", err)
		}
	}
	variants := make([]domain.Variant, 0)
	if imageConfig != nil && !deduplicated {
		widths := []int{320, 768, 1440}
		quality := 82
		if file.Purpose == domain.PurposeAvatar {
			widths = []int{128, 320, 768}
			quality = 92
		}
		variants, err = p.createVariants(ctx, temporaryPath, blob, *imageConfig, widths, quality)
		if err != nil {
			return err
		}
	}
	if err := p.repository.SaveVariants(ctx, job, file.ID, variants); err != nil {
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

// createVariants создаёт WebP-варианты указанных размеров и качества без увеличения исходника.
func (p *Processor) createVariants(ctx context.Context, inputPath string, blob domain.Blob, source image.Config, widths []int, quality int) ([]domain.Variant, error) {
	result := make([]domain.Variant, 0, len(widths))
	for _, width := range widths {
		if width >= source.Width {
			continue
		}
		outputPath := inputPath + "-" + strconv.Itoa(width) + ".webp"
		command := exec.CommandContext(ctx, "vipsthumbnail", inputPath, "--size", strconv.Itoa(width)+"x", "--output", outputPath+"[Q="+strconv.Itoa(quality)+",strip]")
		if output, err := command.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("создать WebP variant %d: %w: %s", width, err, strings.TrimSpace(string(output)))
		}
		variant, err := p.uploadVariant(ctx, outputPath, blob, width, source)
		_ = os.Remove(outputPath)
		if err != nil {
			return nil, err
		}
		result = append(result, variant)
	}

	return result, nil
}

func (p *Processor) uploadVariant(ctx context.Context, path string, blob domain.Blob, width int, source image.Config) (domain.Variant, error) {
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
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return domain.Variant{}, fmt.Errorf("вычислить checksum variant: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return domain.Variant{}, fmt.Errorf("перемотать variant: %w", err)
	}
	name := "w" + strconv.Itoa(width)
	key := filepath.ToSlash("variants/" + blob.ID + "/" + name + ".webp")
	if err := p.storage.Put(ctx, p.config.S3.MediaBucket, key, "image/webp", file, info.Size()); err != nil {
		return domain.Variant{}, fmt.Errorf("загрузить WebP variant: %w", err)
	}
	height := int(float64(source.Height) * float64(width) / float64(source.Width))

	return domain.Variant{ID: uuid.NewString(), BlobID: blob.ID, Name: name, Format: "webp", Width: width, Height: height, SizeBytes: info.Size(), ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)), ObjectKey: key, CreatedAt: time.Now().UTC()}, nil
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
		if int64(config.Width)*int64(config.Height) > maxPixels {
			return "", nil, fmt.Errorf("изображение превышает pixel limit: %w", errImagePixelLimit)
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
