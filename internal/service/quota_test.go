package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/overmindv/media/internal/domain"
)

type partSigningStorage struct {
	fakeStorage
	sizes []int64
}

func (s *partSigningStorage) PresignPart(_ context.Context, _, _, _ string, _ int32, size int64, _ time.Duration) (string, error) {
	s.sizes = append(s.sizes, size)
	return "part-url", nil
}

func TestMultipartPartsCannotExceedReservedSize(t *testing.T) {
	repo := &fakeRepository{
		file: domain.File{ID: "file", OwnerUserID: "owner", Status: domain.StatusPendingUpload},
		session: domain.UploadSession{FileID: "file", Mode: domain.UploadModeMultipart, MultipartID: "fixture",
			ExpectedSize: (5 << 20) + 7, PartSize: 5 << 20, ExpiresAt: time.Now().Add(time.Hour)},
	}
	objects := &partSigningStorage{}
	media := New(repo, objects, testConfig())
	actor := domain.Actor{UserID: "owner"}
	if _, err := media.CreateUploadParts(t.Context(), "file", []int32{1, 3}, actor); err == nil || len(objects.sizes) != 0 {
		t.Error("invalid request should not sign any parts", err, objects.sizes)
	}
	if _, err := media.CreateUploadParts(t.Context(), "file", []int32{1, 2}, actor); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(objects.sizes, []int64{5 << 20, 7}) {
		t.Error("wrong part sizes", objects.sizes)
	}
}

type quotaMultipartStorage struct {
	fakeStorage
	repo        *fakeRepository
	initialized bool
	aborted     bool
}

func (s *quotaMultipartStorage) CreateMultipart(context.Context, string, string, string) (string, error) {
	s.initialized = s.repo.created && s.repo.file.Status == domain.StatusPendingUpload
	return "upload-id", nil
}

func (s *quotaMultipartStorage) AbortMultipart(context.Context, string, string, string) error {
	s.aborted = true
	return nil
}

func TestMultipartInitReservesFirstAndReleasesOnFailure(t *testing.T) {
	failure := errors.New("fixture commit failure")
	repo := &fakeRepository{multipartErr: failure}
	objects := &quotaMultipartStorage{repo: repo}
	media := New(repo, objects, testConfig())
	_, err := media.CreateUpload(t.Context(), domain.CreateUploadInput{
		OriginalName: "fixture.zip", ContentType: "application/zip", SizeBytes: 120 << 20,
		Checksum: strings.Repeat("b", 64), Purpose: domain.PurposeArchive, Visibility: domain.VisibilityPrivate,
	}, domain.Actor{UserID: "11111111-1111-1111-1111-111111111111"})
	if !errors.Is(err, failure) || !objects.initialized || !objects.aborted || repo.file.Status != domain.StatusRejected {
		t.Error("multipart init must reserve before storage and release on failure", objects.initialized, objects.aborted, repo.file.Status, err)
	}
}
