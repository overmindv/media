package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

func TestDownloadACL(t *testing.T) {
	t.Parallel()

	owner := domain.Actor{UserID: "owner"}
	other := domain.Actor{UserID: "other"}
	admin := domain.Actor{UserID: "admin", Roles: []string{"admin"}}
	anonymous := domain.Actor{}
	for _, tc := range []struct {
		name         string
		purpose      domain.Purpose
		visibility   domain.Visibility
		status       domain.Status
		variant      string
		actor        domain.Actor
		wantHTTP     int
		sign, attach bool
	}{
		{"public anonymous original", domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady, "", anonymous, 403, false, false},
		{"public other original", domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady, "original", other, 403, false, false},
		{"public admin original", domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady, "original", admin, 403, false, false},
		{"public owner original", domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady, "original", owner, 200, true, true},
		{"public anonymous preview", domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady, "w128", anonymous, 200, false, false},
		{"private other preview", domain.PurposeContentImage, domain.VisibilityPrivate, domain.StatusReady, "w128", other, 403, false, false},
		{"private owner preview", domain.PurposeContentImage, domain.VisibilityPrivate, domain.StatusReady, "w128", owner, 200, true, false},
		{"private other document", domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady, "", other, 403, false, false},
		{"private admin document", domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady, "", admin, 200, true, true},
		{"public document without ACL", domain.PurposeAttachment, domain.VisibilityPublic, domain.StatusReady, "", anonymous, 403, false, false},
		{"private owner quarantined", domain.PurposeArchive, domain.VisibilityPrivate, domain.StatusQuarantined, "", owner, 409, false, false},
		{"invalid variant", domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady, "../original", owner, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prefix := "variants/private/"
			if tc.visibility == domain.VisibilityPublic {
				prefix = domain.PublicVariantPrefix
			}
			repo := &fakeRepository{
				file:       domain.File{ID: "file", OwnerUserID: owner.UserID, Purpose: tc.purpose, Visibility: tc.visibility, Status: tc.status},
				objectKeys: map[string]string{"w128": prefix + "file/w128.webp"},
			}
			objects := &fakeStorage{}
			cfg := testConfig()
			cfg.DownloadTTL = 5 * time.Minute
			media := New(repo, objects, cfg)
			now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
			media.now = func() time.Time { return now }
			result, err := media.DownloadURL(context.Background(), "file", tc.variant, tc.actor)
			if tc.wantHTTP != http.StatusOK {
				var issue *apperror.Error
				if !errors.As(err, &issue) || issue.Status != tc.wantHTTP || repo.objectKeyCalls != 0 || objects.signed != 0 {
					t.Error("expected refusal before object access", err, repo.objectKeyCalls, objects.signed)
				}

				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.sign {
				if objects.signed != 1 || objects.attachment != tc.attach || objects.ttl != cfg.DownloadTTL || !result.ExpiresAt.Equal(now.Add(cfg.DownloadTTL)) {
					t.Error("unexpected download signature or expiry", result)
				}
			} else if result.URL != cfg.PublicBaseURL+"/"+prefix+"file/w128.webp" || !result.ExpiresAt.IsZero() || objects.signed != 0 {
				t.Error("expected a permanent public WebP URL", result)
			}
			if repo.objectKeyCalls != 1 {
				t.Error("expected one object key lookup")
			}
		})
	}
}

func TestMetadataDoesNotPublishOriginal(t *testing.T) {
	t.Parallel()

	repo := &fakeRepository{file: domain.File{
		ID: "file", Purpose: domain.PurposeAvatar, Visibility: domain.VisibilityPublic, Status: domain.StatusReady,
	}}
	media := New(repo, &fakeStorage{}, testConfig())
	file, err := media.GetFile(context.Background(), "file", domain.Actor{})
	if err != nil || file.PublicURL != "" || repo.objectKeyCalls != 0 {
		t.Error("missing preview must not expose the original", err, file.PublicURL)
	}
}
