package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

const publishedAttachmentTTL = 30 * time.Second

func (s *Service) DownloadPublishedAttachment(ctx context.Context, ref domain.PublishedAttachmentRef) (domain.Download, error) {
	for _, value := range []*string{&ref.MaterialID, &ref.RevisionID, &ref.FileID} {
		id, valid := bindingID(*value)
		if !valid {
			return domain.Download{}, apperror.New(apperror.ValidationError, "некорректный ID вложения", http.StatusBadRequest)
		}
		*value = id
	}
	key, err := s.repository.PublishedAttachmentKey(ctx, ref)
	if err != nil {
		return domain.Download{}, err
	}
	ttl := min(s.config.DownloadTTL, publishedAttachmentTTL)
	expiresAt := s.now().Add(ttl)
	link, err := s.storage.PresignGet(ctx, s.config.S3.MediaBucket, key, ttl, true)
	if err != nil {
		return domain.Download{}, fmt.Errorf("sign published attachment: %w", err)
	}

	return domain.Download{URL: link, ExpiresAt: expiresAt}, nil
}
