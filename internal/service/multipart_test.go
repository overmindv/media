package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

type completedMultipart struct {
	fakeStorage
	completions int
}

func (s *completedMultipart) CompleteMultipart(context.Context, string, string, string, []domain.CompletedPart) error {
	s.completions++

	return ErrMultipartMissing
}

func TestCompleteRecoversAfterMultipartWasCommitted(t *testing.T) {
	t.Parallel()

	repo := &fakeRepository{
		file: domain.File{ID: "file", OwnerUserID: "owner", Status: domain.StatusPendingUpload},
		session: domain.UploadSession{FileID: "file", Mode: domain.UploadModeMultipart, MultipartID: "id",
			ExpectedSize: 10, Checksum: "known", ExpiresAt: time.Now().Add(time.Hour)},
	}
	objects := &completedMultipart{fakeStorage: fakeStorage{info: ObjectInfo{SizeBytes: 11}}}
	media := New(repo, objects, testConfig())
	actor := domain.Actor{UserID: "owner"}
	_, err := media.CompleteUpload(t.Context(), "file", nil, actor)
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Code != apperror.UploadIntegrityFailed || repo.file.Status != domain.StatusPendingUpload {
		t.Error("multipart recovery bypassed the integrity check", err)
	}
	objects.info.SizeBytes = 10
	for range 2 {
		if file, err := media.CompleteUpload(t.Context(), "file", nil, actor); err != nil || file.Status != domain.StatusQuarantined {
			t.Fatal("recover multipart", file.Status, err)
		}
	}
	if objects.completions != 2 {
		t.Error("repeat complete issued an additional storage request", objects.completions)
	}
}
