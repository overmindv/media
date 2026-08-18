package service

import (
	"context"
	"io"
	"time"

	"github.com/overmindv/media/internal/domain"
)

type Repository interface {
	Ping(context.Context) error
	ActiveSize(context.Context, string) (int64, error)
	CreateUpload(context.Context, domain.File, domain.UploadSession) error
	GetFile(context.Context, string) (domain.File, error)
	GetUploadSession(context.Context, string) (domain.UploadSession, error)
	GetObjectKey(context.Context, string, string) (string, error)
	CompleteUpload(context.Context, string, string, int64, string) (domain.File, error)
	ListFiles(context.Context, string, string, int, int) (domain.FileList, error)
	SoftDeleteFile(context.Context, string, time.Time) error
	ResolvePublicFiles(context.Context, []string, []string) ([]domain.PublicFile, error)
	ReplaceAvatarBinding(context.Context, string, *string) error
}

type ObjectInfo struct {
	SizeBytes   int64
	Checksum    string
	ContentType string
}

type ObjectStorage interface {
	Ping(context.Context) error
	PresignPost(context.Context, string, string, string, int64, time.Duration) (string, map[string]string, error)
	CreateMultipart(context.Context, string, string, string) (string, error)
	PresignPart(context.Context, string, string, string, int32, time.Duration) (string, error)
	CompleteMultipart(context.Context, string, string, string, []domain.CompletedPart) error
	Head(context.Context, string, string) (ObjectInfo, error)
	PresignGet(context.Context, string, string, time.Duration) (string, error)
	Get(context.Context, string, string) (io.ReadCloser, error)
	Put(context.Context, string, string, string, io.Reader, int64) error
	Copy(context.Context, string, string, string, string, string) error
	Delete(context.Context, string, string) error
}
