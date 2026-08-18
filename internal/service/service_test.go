package service

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
)

type fakeRepository struct {
	file    domain.File
	session domain.UploadSession
	created bool
	active  int64
	public  []domain.PublicFile
}

func (f *fakeRepository) Ping(context.Context) error { return nil }
func (f *fakeRepository) ActiveSize(context.Context, string) (int64, error) {
	return f.active, nil
}
func (f *fakeRepository) CreateUpload(_ context.Context, file domain.File, session domain.UploadSession) error {
	f.file, f.session, f.created = file, session, true
	return nil
}
func (f *fakeRepository) GetFile(context.Context, string) (domain.File, error) { return f.file, nil }
func (f *fakeRepository) GetUploadSession(context.Context, string) (domain.UploadSession, error) {
	return f.session, nil
}
func (f *fakeRepository) GetObjectKey(context.Context, string, string) (string, error) {
	return "originals/file", nil
}
func (f *fakeRepository) CompleteUpload(_ context.Context, _ string, contentType string, size int64, checksum string) (domain.File, error) {
	f.file.Status, f.file.DetectedContentType, f.file.SizeBytes, f.file.ChecksumSHA256 = domain.StatusQuarantined, contentType, size, checksum
	return f.file, nil
}
func (f *fakeRepository) ListFiles(context.Context, string, string, int, int) (domain.FileList, error) {
	return domain.FileList{Items: []domain.File{f.file}}, nil
}
func (f *fakeRepository) SoftDeleteFile(context.Context, string, time.Time) error { return nil }
func (f *fakeRepository) ResolvePublicFiles(context.Context, []string, []string) ([]domain.PublicFile, error) {
	return f.public, nil
}

// TestCreateUploadEnforcesAvatarPolicy проверяет отдельные ограничения avatar upload.
func TestCreateUploadEnforcesAvatarPolicy(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		sizeBytes   int64
		visibility  domain.Visibility
	}{
		{name: "private", contentType: "image/jpeg", sizeBytes: 1024, visibility: domain.VisibilityPrivate},
		{name: "pdf", contentType: "application/pdf", sizeBytes: 1024, visibility: domain.VisibilityPublic},
		{name: "too large", contentType: "image/webp", sizeBytes: (5 << 20) + 1, visibility: domain.VisibilityPublic},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			media := New(&fakeRepository{}, &fakeStorage{}, testConfig())
			_, err := media.CreateUpload(context.Background(), domain.CreateUploadInput{
				OriginalName: "avatar",
				ContentType:  test.contentType,
				SizeBytes:    test.sizeBytes,
				Checksum:     strings.Repeat("e", 64),
				Purpose:      domain.PurposeAvatar,
				Visibility:   test.visibility,
			}, domain.Actor{UserID: "11111111-1111-1111-1111-111111111111"})
			if err == nil {
				t.Fatal("нарушение avatar policy должно быть отклонено")
			}
		})
	}
}

// TestResolvePublicFilesBuildsVariantURLs проверяет batch resolution без дополнительных storage-вызовов.
func TestResolvePublicFilesBuildsVariantURLs(t *testing.T) {
	repository := &fakeRepository{public: []domain.PublicFile{{
		FileID: "file-id",
		URLs: map[string]string{
			"w128": "variants/file/w128.webp",
			"w320": "variants/file/w320.webp",
		},
	}}}
	media := New(repository, &fakeStorage{}, testConfig())
	files, err := media.ResolvePublicFiles(context.Background(), []string{"file-id"}, []string{"w128", "w320"})
	if err != nil {
		t.Fatalf("разрешить публичные файлы: %v", err)
	}
	if len(files) != 1 || files[0].URLs["w128"] != "https://cdn.example/variants/file/w128.webp" {
		t.Fatalf("неожиданный batch result: %+v", files)
	}
}

// TestValidateAvatarChecksOwnershipAndState проверяет узкий Users-контракт без доверия actor headers.
func TestValidateAvatarChecksOwnershipAndState(t *testing.T) {
	userID := "11111111-1111-1111-1111-111111111111"
	fileID := "22222222-2222-2222-2222-222222222222"
	repository := &fakeRepository{file: domain.File{
		ID:          fileID,
		OwnerUserID: userID,
		Purpose:     domain.PurposeAvatar,
		Visibility:  domain.VisibilityPublic,
		Status:      domain.StatusReady,
	}}
	media := New(repository, &fakeStorage{}, testConfig())
	if err := media.ValidateAvatar(context.Background(), userID, fileID); err != nil {
		t.Fatalf("готовый avatar должен пройти проверку: %v", err)
	}
	if err := media.ValidateAvatar(context.Background(), "33333333-3333-3333-3333-333333333333", fileID); err == nil {
		t.Fatal("чужой avatar должен быть отклонён")
	}
	repository.file.Status = domain.StatusProcessing
	if err := media.ValidateAvatar(context.Background(), userID, fileID); err == nil {
		t.Fatal("неготовый avatar должен быть отклонён")
	}
}
func (f *fakeRepository) ReplaceAvatarBinding(context.Context, string, *string) error {
	return nil
}

type fakeStorage struct {
	info ObjectInfo
}

func (f *fakeStorage) Ping(context.Context) error { return nil }
func (f *fakeStorage) PresignPost(context.Context, string, string, string, int64, time.Duration) (string, map[string]string, error) {
	return "https://storage/upload", map[string]string{"key": "value"}, nil
}
func (f *fakeStorage) CreateMultipart(context.Context, string, string, string) (string, error) {
	return "upload-id", nil
}
func (f *fakeStorage) PresignPart(context.Context, string, string, string, int32, time.Duration) (string, error) {
	return "https://storage/part", nil
}
func (f *fakeStorage) CompleteMultipart(context.Context, string, string, string, []domain.CompletedPart) error {
	return nil
}
func (f *fakeStorage) Head(context.Context, string, string) (ObjectInfo, error) { return f.info, nil }
func (f *fakeStorage) PresignGet(context.Context, string, string, time.Duration) (string, error) {
	return "https://storage/get", nil
}
func (f *fakeStorage) Get(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("data")), nil
}
func (f *fakeStorage) Put(context.Context, string, string, string, io.Reader, int64) error {
	return nil
}
func (f *fakeStorage) Copy(context.Context, string, string, string, string, string) error { return nil }
func (f *fakeStorage) Delete(context.Context, string, string) error                       { return nil }

func testConfig() config.Config {
	return config.Config{
		UploadTTL:      15 * time.Minute,
		SessionTTL:     24 * time.Hour,
		DownloadTTL:    5 * time.Minute,
		PublicBaseURL:  "https://cdn.example",
		UserQuotaBytes: 5 << 30,
		S3:             config.S3{QuarantineBucket: "quarantine", MediaBucket: "media"},
		Limits:         config.Limits{AvatarBytes: 5 << 20, MaxAvatarPixels: 12_000_000, ImageBytes: 20 << 20, DocumentBytes: 50 << 20, ArchiveBytes: 250 << 20, MultipartThreshold: 100 << 20, PartSize: 16 << 20},
	}
}

// TestCreateUploadSelectsMode проверяет границу single и multipart upload.
func TestCreateUploadSelectsMode(t *testing.T) {
	repository := &fakeRepository{}
	storage := &fakeStorage{}
	media := New(repository, storage, testConfig())
	input := domain.CreateUploadInput{
		OriginalName: "photo.jpg", ContentType: "image/jpeg", SizeBytes: 1024,
		Checksum: strings.Repeat("a", 64), Purpose: domain.PurposeAvatar, Visibility: domain.VisibilityPublic,
	}
	target, err := media.CreateUpload(context.Background(), input, domain.Actor{UserID: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatalf("создать single upload: %v", err)
	}
	if !repository.created || target.Mode != domain.UploadModeSingle || target.URL == "" {
		t.Fatalf("неожиданный single target: %+v", target)
	}
	input.Purpose, input.Visibility, input.ContentType = domain.PurposeArchive, domain.VisibilityPrivate, "application/zip"
	input.SizeBytes = 120 << 20
	target, err = media.CreateUpload(context.Background(), input, domain.Actor{UserID: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatalf("создать multipart upload: %v", err)
	}
	if target.Mode != domain.UploadModeMultipart || target.MultipartID == "" {
		t.Fatalf("неожиданный multipart target: %+v", target)
	}
}

// TestCompleteUploadIsIdempotent проверяет повторный Complete без второго обращения к storage.
func TestCompleteUploadIsIdempotent(t *testing.T) {
	checksum := strings.Repeat("b", 64)
	repository := &fakeRepository{
		file:    domain.File{ID: "file-id", OwnerUserID: "owner", Status: domain.StatusPendingUpload},
		session: domain.UploadSession{FileID: "file-id", Mode: domain.UploadModeSingle, ExpectedSize: 10, Checksum: checksum, ExpiresAt: time.Now().Add(time.Hour)},
	}
	storage := &fakeStorage{info: ObjectInfo{SizeBytes: 10, Checksum: checksum, ContentType: "application/pdf"}}
	media := New(repository, storage, testConfig())
	file, err := media.CompleteUpload(context.Background(), "file-id", nil, domain.Actor{UserID: "owner"})
	if err != nil || file.Status != domain.StatusQuarantined {
		t.Fatalf("завершить upload: file=%+v err=%v", file, err)
	}
	file, err = media.CompleteUpload(context.Background(), "file-id", nil, domain.Actor{UserID: "owner"})
	if err != nil || file.Status != domain.StatusQuarantined {
		t.Fatalf("повторить upload: file=%+v err=%v", file, err)
	}
}

// TestCreateUploadRejectsPublicArchive проверяет policy приватных вложений.
func TestCreateUploadRejectsPublicArchive(t *testing.T) {
	media := New(&fakeRepository{}, &fakeStorage{}, testConfig())
	_, err := media.CreateUpload(context.Background(), domain.CreateUploadInput{
		OriginalName: "data.zip", ContentType: "application/zip", SizeBytes: 10,
		Checksum: strings.Repeat("c", 64), Purpose: domain.PurposeArchive, Visibility: domain.VisibilityPublic,
	}, domain.Actor{UserID: "11111111-1111-1111-1111-111111111111"})
	if err == nil {
		t.Fatal("публичный архив должен быть отклонён")
	}
}

// TestCreateUploadRejectsExceededQuota проверяет суммарную пользовательскую квоту до создания presigned URL.
func TestCreateUploadRejectsExceededQuota(t *testing.T) {
	cfg := testConfig()
	repository := &fakeRepository{
		active: cfg.UserQuotaBytes - 5,
	}
	media := New(repository, &fakeStorage{}, cfg)
	_, err := media.CreateUpload(context.Background(), domain.CreateUploadInput{
		OriginalName: "photo.jpg",
		ContentType:  "image/jpeg",
		SizeBytes:    10,
		Checksum:     strings.Repeat("d", 64),
		Purpose:      domain.PurposeAvatar,
		Visibility:   domain.VisibilityPublic,
	}, domain.Actor{
		UserID: "11111111-1111-1111-1111-111111111111",
	})
	if err == nil || repository.created {
		t.Fatal("upload сверх пользовательской квоты должен быть отклонён")
	}
}
