package service

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
)

type Service struct {
	repository Repository
	storage    ObjectStorage
	config     config.Config
	now        func() time.Time
}

// New создаёт Media service из repository, object storage и конфигурации.
func New(repository Repository, storage ObjectStorage, cfg config.Config) *Service {
	return &Service{
		repository: repository,
		storage:    storage,
		config:     cfg,
		now:        time.Now,
	}
}

// Ready проверяет обязательные зависимости API.
func (s *Service) Ready(ctx context.Context) error {
	if err := s.repository.Ping(ctx); err != nil {
		return fmt.Errorf("проверить PostgreSQL: %w", err)
	}
	if err := s.storage.Ping(ctx); err != nil {
		return fmt.Errorf("проверить object storage: %w", err)
	}

	return nil
}

// CreateUpload валидирует декларацию файла и создаёт прямую upload-сессию.
func (s *Service) CreateUpload(ctx context.Context, input domain.CreateUploadInput, actor domain.Actor) (domain.UploadTarget, error) {
	if actor.UserID == "" || uuid.Validate(actor.UserID) != nil {
		return domain.UploadTarget{}, apperror.New(apperror.PermissionDenied, "требуется авторизация", http.StatusForbidden)
	}
	if err := s.validateInput(input); err != nil {
		return domain.UploadTarget{}, err
	}
	activeSize, err := s.repository.ActiveSize(ctx, actor.UserID)
	if err != nil {
		return domain.UploadTarget{}, fmt.Errorf("получить использованную media quota: %w", err)
	}
	if input.SizeBytes > s.config.UserQuotaBytes-activeSize {
		return domain.UploadTarget{}, apperror.New(apperror.FileTooLarge, "превышена квота хранилища пользователя", http.StatusRequestEntityTooLarge)
	}
	fileID := uuid.NewString()
	now := s.now().UTC()
	expiresAt := now.Add(s.config.UploadTTL)
	objectKey := fmt.Sprintf("uploads/%s/%s", now.Format("2006/01/02"), fileID)
	mode := domain.UploadModeSingle
	if input.SizeBytes >= s.config.Limits.MultipartThreshold {
		mode = domain.UploadModeMultipart
	}
	file := domain.File{
		ID:                  fileID,
		OwnerUserID:         actor.UserID,
		Purpose:             input.Purpose,
		Visibility:          input.Visibility,
		OriginalName:        filepath.Base(input.OriginalName),
		DeclaredContentType: input.ContentType,
		SizeBytes:           input.SizeBytes,
		ChecksumSHA256:      strings.ToLower(input.Checksum),
		Status:              domain.StatusPendingUpload,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	session := domain.UploadSession{
		FileID:       fileID,
		Mode:         mode,
		Bucket:       s.config.S3.QuarantineBucket,
		ObjectKey:    objectKey,
		ExpectedSize: input.SizeBytes,
		Checksum:     strings.ToLower(input.Checksum),
		ContentType:  input.ContentType,
		ExpiresAt:    now.Add(s.config.SessionTTL),
	}
	target := domain.UploadTarget{
		FileID:    fileID,
		Mode:      mode,
		PartSize:  s.config.Limits.PartSize,
		ExpiresAt: expiresAt,
	}
	if mode == domain.UploadModeSingle {
		url, fields, err := s.storage.PresignPost(ctx, session.Bucket, objectKey, input.ContentType, input.SizeBytes, s.config.UploadTTL)
		if err != nil {
			return domain.UploadTarget{}, fmt.Errorf("создать presigned POST: %w", err)
		}
		target.URL = url
		target.Fields = fields
	} else {
		uploadID, err := s.storage.CreateMultipart(ctx, session.Bucket, objectKey, input.ContentType)
		if err != nil {
			return domain.UploadTarget{}, fmt.Errorf("создать multipart upload: %w", err)
		}
		session.MultipartID = uploadID
		target.MultipartID = uploadID
	}
	if err := s.repository.CreateUpload(ctx, file, session); err != nil {
		return domain.UploadTarget{}, fmt.Errorf("сохранить upload-сессию: %w", err)
	}

	return target, nil
}

// CreateUploadParts выдаёт URL только владельцу активной multipart-сессии.
func (s *Service) CreateUploadParts(ctx context.Context, fileID string, partNumbers []int32, actor domain.Actor) ([]domain.UploadPart, error) {
	file, session, err := s.ownedSession(ctx, fileID, actor)
	if err != nil {
		return nil, err
	}
	_ = file
	if session.Mode != domain.UploadModeMultipart || session.MultipartID == "" {
		return nil, apperror.New(apperror.ValidationError, "файл не использует multipart upload", http.StatusBadRequest)
	}
	if s.now().After(session.ExpiresAt) {
		return nil, apperror.New(apperror.UploadExpired, "upload-сессия истекла", http.StatusGone)
	}
	result := make([]domain.UploadPart, 0, len(partNumbers))
	for _, partNumber := range partNumbers {
		if partNumber < 1 || partNumber > 10_000 {
			return nil, apperror.New(apperror.ValidationError, "некорректный номер части", http.StatusBadRequest)
		}
		url, err := s.storage.PresignPart(ctx, session.Bucket, session.ObjectKey, session.MultipartID, partNumber, s.config.UploadTTL)
		if err != nil {
			return nil, fmt.Errorf("подписать multipart part %d: %w", partNumber, err)
		}
		result = append(result, domain.UploadPart{
			PartNumber: partNumber,
			URL:        url,
			ExpiresAt:  s.now().Add(s.config.UploadTTL),
		})
	}

	return result, nil
}

// CompleteUpload завершает multipart при необходимости, проверяет объект и ставит scan job.
func (s *Service) CompleteUpload(ctx context.Context, fileID string, parts []domain.CompletedPart, actor domain.Actor) (domain.File, error) {
	file, session, err := s.ownedSession(ctx, fileID, actor)
	if err != nil {
		return domain.File{}, err
	}
	if file.Status != domain.StatusPendingUpload {
		return file, nil
	}
	if s.now().After(session.ExpiresAt) {
		return domain.File{}, apperror.New(apperror.UploadExpired, "upload-сессия истекла", http.StatusGone)
	}
	if session.Mode == domain.UploadModeMultipart {
		if err := s.storage.CompleteMultipart(ctx, session.Bucket, session.ObjectKey, session.MultipartID, parts); err != nil {
			return domain.File{}, fmt.Errorf("завершить multipart upload: %w", err)
		}
	}
	info, err := s.storage.Head(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		return domain.File{}, fmt.Errorf("проверить загруженный объект: %w", err)
	}
	if info.SizeBytes != session.ExpectedSize || (info.Checksum != "" && !strings.EqualFold(info.Checksum, session.Checksum)) {
		return domain.File{}, apperror.New(apperror.UploadIntegrityFailed, "размер или checksum файла не совпадает", http.StatusUnprocessableEntity)
	}
	result, err := s.repository.CompleteUpload(ctx, fileID, info.ContentType, info.SizeBytes, session.Checksum)
	if err != nil {
		return domain.File{}, fmt.Errorf("зафиксировать завершение upload: %w", err)
	}

	return result, nil
}

// GetFile возвращает метаданные файла после проверки доступа.
func (s *Service) GetFile(ctx context.Context, id string, actor domain.Actor) (domain.File, error) {
	file, err := s.repository.GetFile(ctx, id)
	if err != nil {
		return domain.File{}, err
	}
	if !file.CanRead(actor) {
		return domain.File{}, apperror.New(apperror.PermissionDenied, "нет доступа к файлу", http.StatusForbidden)
	}
	if file.Visibility == domain.VisibilityPublic && file.Status == domain.StatusReady {
		key, keyErr := s.repository.GetObjectKey(ctx, file.ID, "original")
		if keyErr != nil {
			return domain.File{}, fmt.Errorf("получить public object key: %w", keyErr)
		}
		file.PublicURL = s.config.PublicBaseURL + "/" + key
	}

	return file, nil
}

// ListFiles возвращает страницу файлов текущего пользователя.
func (s *Service) ListFiles(ctx context.Context, actor domain.Actor, status string, limit, offset int) (domain.FileList, error) {
	if actor.UserID == "" {
		return domain.FileList{}, apperror.New(apperror.PermissionDenied, "требуется авторизация", http.StatusForbidden)
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	if status != "" {
		valid := map[string]bool{"pending_upload": true, "quarantined": true, "processing": true, "ready": true, "rejected": true, "deleted": true}
		if !valid[status] {
			return domain.FileList{}, apperror.New(apperror.ValidationError, "неизвестный статус файла", http.StatusBadRequest)
		}
	}

	return s.repository.ListFiles(ctx, actor.UserID, status, limit, offset)
}

// DownloadURL создаёт короткую ссылку на готовый приватный объект.
func (s *Service) DownloadURL(ctx context.Context, id, variant string, actor domain.Actor) (domain.Download, error) {
	file, err := s.GetFile(ctx, id, actor)
	if err != nil {
		return domain.Download{}, err
	}
	if file.Status != domain.StatusReady {
		return domain.Download{}, apperror.New(apperror.FileNotReady, "файл ещё не готов", http.StatusConflict)
	}
	if file.Visibility == domain.VisibilityPublic {
		key, keyErr := s.repository.GetObjectKey(ctx, file.ID, variant)
		if keyErr != nil {
			return domain.Download{}, keyErr
		}

		return domain.Download{URL: s.config.PublicBaseURL + "/" + key}, nil
	}
	key, err := s.repository.GetObjectKey(ctx, file.ID, variant)
	if err != nil {
		return domain.Download{}, err
	}
	url, err := s.storage.PresignGet(ctx, s.config.S3.MediaBucket, key, s.config.DownloadTTL)
	if err != nil {
		return domain.Download{}, fmt.Errorf("создать download URL: %w", err)
	}

	return domain.Download{
		URL:       url,
		ExpiresAt: s.now().Add(s.config.DownloadTTL),
	}, nil
}

// DeleteFile выполняет soft delete с retention period.
func (s *Service) DeleteFile(ctx context.Context, id string, actor domain.Actor) error {
	file, err := s.repository.GetFile(ctx, id)
	if err != nil {
		return err
	}
	if !file.CanDelete(actor) {
		return apperror.New(apperror.PermissionDenied, "нет права удалить файл", http.StatusForbidden)
	}
	if err := s.repository.SoftDeleteFile(ctx, id, s.now().Add(s.config.DeleteRetention)); err != nil {
		return fmt.Errorf("удалить файл: %w", err)
	}

	return nil
}

// ResolvePublicFiles возвращает CDN URL нескольких готовых публичных файлов без N+1 запросов.
func (s *Service) ResolvePublicFiles(ctx context.Context, ids, variants []string) ([]domain.PublicFile, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, apperror.New(apperror.ValidationError, "требуется от 1 до 100 file IDs", http.StatusBadRequest)
	}
	allowedVariants := map[string]bool{
		"w128": true,
		"w320": true,
		"w768": true,
	}
	for _, variant := range variants {
		if !allowedVariants[variant] {
			return nil, apperror.New(apperror.ValidationError, "неподдерживаемый public variant", http.StatusBadRequest)
		}
	}
	files, err := s.repository.ResolvePublicFiles(ctx, ids, variants)
	if err != nil {
		return nil, fmt.Errorf("разрешить public files: %w", err)
	}
	for index := range files {
		for variant, key := range files[index].URLs {
			files[index].URLs[variant] = s.config.PublicBaseURL + "/" + key
		}
	}

	return files, nil
}

// ReplaceAvatarBinding идемпотентно устанавливает или снимает binding аватара пользователя.
func (s *Service) ReplaceAvatarBinding(ctx context.Context, userID string, fileID *string) error {
	if uuid.Validate(userID) != nil || (fileID != nil && uuid.Validate(*fileID) != nil) {
		return apperror.New(apperror.ValidationError, "user_id и file_id должны быть UUID", http.StatusBadRequest)
	}
	if err := s.repository.ReplaceAvatarBinding(ctx, userID, fileID); err != nil {
		return fmt.Errorf("заменить avatar binding: %w", err)
	}

	return nil
}

// ValidateAvatar проверяет владельца и invariant готового публичного avatar-файла.
func (s *Service) ValidateAvatar(ctx context.Context, userID, fileID string) error {
	if uuid.Validate(userID) != nil || uuid.Validate(fileID) != nil {
		return apperror.New(apperror.ValidationError, "user_id и file_id должны быть UUID", http.StatusBadRequest)
	}
	file, err := s.repository.GetFile(ctx, fileID)
	if err != nil {
		return fmt.Errorf("получить avatar file: %w", err)
	}
	if file.OwnerUserID != userID {
		return apperror.New(apperror.PermissionDenied, "файл не принадлежит пользователю", http.StatusForbidden)
	}
	if file.Purpose != domain.PurposeAvatar || file.Visibility != domain.VisibilityPublic || file.Status != domain.StatusReady {
		return apperror.New(apperror.FileNotReady, "файл не является готовым публичным аватаром", http.StatusConflict)
	}

	return nil
}

func (s *Service) ownedSession(ctx context.Context, id string, actor domain.Actor) (domain.File, domain.UploadSession, error) {
	file, err := s.repository.GetFile(ctx, id)
	if err != nil {
		return domain.File{}, domain.UploadSession{}, err
	}
	if actor.UserID == "" || (file.OwnerUserID != actor.UserID && !actor.IsAdmin()) {
		return domain.File{}, domain.UploadSession{}, apperror.New(apperror.PermissionDenied, "нет доступа к upload-сессии", http.StatusForbidden)
	}
	session, err := s.repository.GetUploadSession(ctx, id)
	if err != nil {
		return domain.File{}, domain.UploadSession{}, err
	}

	return file, session, nil
}

func (s *Service) validateInput(input domain.CreateUploadInput) error {
	if input.OriginalName == "" || input.SizeBytes <= 0 || input.ContentType == "" {
		return apperror.New(apperror.ValidationError, "имя, content type и размер обязательны", http.StatusBadRequest)
	}
	checksum, err := hex.DecodeString(input.Checksum)
	if err != nil || len(checksum) != 32 {
		return apperror.New(apperror.ValidationError, "checksum_sha256 должен быть SHA-256 в hex", http.StatusBadRequest)
	}
	limits := map[domain.Purpose]int64{
		domain.PurposeAvatar:       s.config.Limits.AvatarBytes,
		domain.PurposeCatalogLogo:  s.config.Limits.ImageBytes,
		domain.PurposeContentImage: s.config.Limits.ImageBytes,
		domain.PurposeAttachment:   s.config.Limits.DocumentBytes,
		domain.PurposeArchive:      s.config.Limits.ArchiveBytes,
	}
	limit, ok := limits[input.Purpose]
	if !ok {
		return apperror.New(apperror.ValidationError, "неизвестное назначение файла", http.StatusBadRequest)
	}
	if input.SizeBytes > limit {
		return apperror.New(apperror.FileTooLarge, "файл превышает допустимый размер", http.StatusRequestEntityTooLarge)
	}
	if input.Visibility != domain.VisibilityPublic && input.Visibility != domain.VisibilityPrivate {
		return apperror.New(apperror.ValidationError, "неизвестная видимость файла", http.StatusBadRequest)
	}
	if (input.Purpose == domain.PurposeAttachment || input.Purpose == domain.PurposeArchive) && input.Visibility != domain.VisibilityPrivate {
		return apperror.New(apperror.ValidationError, "вложения и архивы должны быть приватными", http.StatusBadRequest)
	}
	allowed := map[string]bool{
		"image/jpeg": true, "image/png": true, "image/webp": true,
		"application/pdf": true, "application/zip": true, "application/gzip": true,
		"application/x-tar": true,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	}
	if !allowed[input.ContentType] {
		return apperror.New(apperror.UnsupportedMediaType, "тип файла не поддерживается", http.StatusUnsupportedMediaType)
	}
	if input.Purpose == domain.PurposeAvatar {
		if input.Visibility != domain.VisibilityPublic {
			return apperror.New(apperror.ValidationError, "аватар должен быть публичным", http.StatusBadRequest)
		}
		if input.ContentType != "image/jpeg" && input.ContentType != "image/png" && input.ContentType != "image/webp" {
			return apperror.New(apperror.UnsupportedMediaType, "аватар должен быть JPEG, PNG или WebP", http.StatusUnsupportedMediaType)
		}
	}

	return nil
}
