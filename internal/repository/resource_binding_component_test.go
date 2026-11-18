//go:build component

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/httpapi"
	"github.com/overmindv/media/internal/service"
)

func bindingFile(t *testing.T, ctx context.Context, repo *Postgres, owner string, purpose domain.Purpose, visibility domain.Visibility, status domain.Status) string {
	t.Helper()
	blob, file := uuid.NewString(), uuid.NewString()
	_, err := repo.pool.Exec(ctx, `
		WITH made AS (
		    INSERT INTO blobs (id, checksum_sha256, size_bytes, visibility, detected_content_type, object_key, scan_status)
		    VALUES ($1, $2, 16, $3, 'image/png', $2, 'clean') RETURNING id
		)
		INSERT INTO files (id, blob_id, owner_user_id, purpose, visibility, original_name, declared_content_type,
		                   size_bytes, checksum_sha256, status)
		SELECT $4, id, $5, $6, $3, 'fixture.png', 'image/png', 16, $2, $7 FROM made
	`, blob, "fixture-"+blob, visibility, file, owner, purpose, status)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestResourceBindingsHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	repo := componentRepository(t)
	router := http.NewServeMux()
	httpapi.Register(router, service.New(repo, nil, config.Config{}), map[string]string{
		"content": "content-fixture", "entities": "entities-fixture", "gateway": "gateway-fixture", "users": "",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	owner, contributor, material, first, next := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	private := bindingFile(t, ctx, repo, owner, domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady)
	image := bindingFile(t, ctx, repo, owner, domain.PurposeContentImage, domain.VisibilityPublic, domain.StatusReady)
	other := bindingFile(t, ctx, repo, contributor, domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady)
	processing := bindingFile(t, ctx, repo, owner, domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusProcessing)
	logo := bindingFile(t, ctx, repo, owner, domain.PurposeCatalogLogo, domain.VisibilityPublic, domain.StatusReady)
	privateLogo := bindingFile(t, ctx, repo, owner, domain.PurposeCatalogLogo, domain.VisibilityPrivate, domain.StatusReady)
	const check = "/v1/internal/content/binding-files/check"
	path := func(materialID, revisionID string) string {
		return "/v1/internal/content/materials/" + materialID + "/revisions/" + revisionID + "/bindings"
	}
	write := func(method, route, token string, body map[string]any, want int, wantCode string) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, route, strings.NewReader(string(data))).WithContext(ctx)
		if token != "" {
			req.Header.Set("X-Media-Service-Token", token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: got %d %s, want %d", method, route, rec.Code, rec.Body.String(), want)
		}
		if wantCode != "" {
			var e apperror.Error
			if err = json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != wantCode {
				t.Errorf("error code: got %s, want %s", e.Code, wantCode)
			}
		}
	}
	cmd := func(actor string, version int64, ids ...string) map[string]any {
		return map[string]any{"actor_user_id": actor, "file_ids": append([]string{}, ids...), "version": version}
	}
	for _, token := range []string{"", "entities-fixture", "gateway-fixture"} {
		write(http.MethodPut, path(material, first), token, cmd(owner, 1, private), http.StatusForbidden, apperror.PermissionDenied)
	}
	for _, id := range []string{other, processing, logo, uuid.NewString()} {
		write(http.MethodPost, check, "content-fixture", map[string]any{"actor_user_id": owner, "file_ids": []string{id}},
			http.StatusConflict, apperror.FileNotReady)
	}
	write(http.MethodPost, check, "content-fixture", map[string]any{"actor_user_id": owner, "file_ids": []string{private, image}},
		http.StatusNoContent, "")
	write(http.MethodPut, path(material, first), "content-fixture", map[string]any{"actor_user_id": owner, "version": 1},
		http.StatusBadRequest, apperror.ValidationError)
	write(http.MethodPut, path(material, first), "content-fixture", cmd(owner, 1, private, private), http.StatusBadRequest, apperror.ValidationError)
	write(http.MethodPut, path(material, first), "content-fixture", cmd(owner, 1, private, image), http.StatusNoContent, "")
	write(http.MethodPut, path(material, first), "content-fixture", cmd(owner, 1, image, private), http.StatusNoContent, "")
	write(http.MethodPut, path(material, first), "content-fixture", cmd(owner, 1, image), http.StatusConflict, apperror.BindingVersionConflict)
	inherited := cmd(contributor, 1, private, other)
	inherited["inherit_from_revision_id"] = first
	write(http.MethodPut, path(uuid.NewString(), next), "content-fixture", inherited, http.StatusConflict, apperror.FileNotReady)
	write(http.MethodPut, path(material, next), "content-fixture", inherited, http.StatusNoContent, "")

	write(http.MethodPut, path(material, first), "content-fixture", cmd(owner, 2), http.StatusNoContent, "")
	write(http.MethodPut, path(material, first), "content-fixture", cmd(owner, 1, private, image), http.StatusNoContent, "")
	var active int
	err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM file_bindings WHERE file_id = $1 AND deleted_at IS NULL`, private).Scan(&active)
	if err != nil || active != 1 {
		t.Errorf("expected just one binding after stale replay: %d %v", active, err)
	}
	purge := time.Now().Add(time.Hour)
	if err := repo.SoftDeleteFile(ctx, private, purge); errorCode(err) != apperror.FileInUse {
		t.Errorf("shared file can be deleted: %v", err)
	}
	write(http.MethodPut, path(material, next), "content-fixture", cmd(contributor, 2), http.StatusNoContent, "")
	if err := repo.SoftDeleteFile(ctx, private, purge); err != nil {
		t.Errorf("last reference did not release the file: %v", err)
	}
	write(http.MethodPut, path(material, next), "content-fixture", inherited, http.StatusNoContent, "")

	logoPath := "/v1/internal/entities/logos/" + uuid.NewString() + "/bindings"
	write(http.MethodPut, logoPath, "content-fixture", cmd(owner, 1, logo), http.StatusForbidden, apperror.PermissionDenied)
	write(http.MethodPut, logoPath, "entities-fixture", cmd(owner, 1, privateLogo), http.StatusConflict, apperror.FileNotReady)
	write(http.MethodPut, logoPath, "entities-fixture", cmd(contributor, 1, logo), http.StatusConflict, apperror.FileNotReady)
	write(http.MethodPut, logoPath, "entities-fixture", cmd(owner, 1, logo), http.StatusNoContent, "")
	if err := repo.SoftDeleteFile(ctx, logo, purge); errorCode(err) != apperror.FileInUse {
		t.Errorf("catalog logo is not protected: %v", err)
	}
	write(http.MethodPut, logoPath, "entities-fixture", cmd(owner, 3), http.StatusNoContent, "")
	write(http.MethodPut, logoPath, "entities-fixture", cmd(owner, 2, logo), http.StatusNoContent, "")
	if err := repo.SoftDeleteFile(ctx, logo, purge); err != nil {
		t.Errorf("stale request restored catalog logo: %v", err)
	}
}

func TestResourceBindingAndDeleteRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	repo := componentRepository(t)
	owner := uuid.NewString()
	for range 8 {
		file := bindingFile(t, ctx, repo, owner, domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady)
		binding := domain.ResourceBinding{
			Scope: domain.BindingContentRevision, ResourceID: uuid.NewString(), EntityID: uuid.NewString(), Version: 1,
			BindingFiles: domain.BindingFiles{ActorUserID: owner, FileIDs: []string{file}},
		}
		start := make(chan struct{})
		bound, deleted := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			bound <- repo.ReplaceResourceBinding(ctx, binding)
		}()
		go func() {
			<-start
			deleted <- repo.SoftDeleteFile(ctx, file, time.Now().Add(time.Hour))
		}()
		close(start)
		bindErr, deleteErr := <-bound, <-deleted
		switch {
		case bindErr == nil && errorCode(deleteErr) == apperror.FileInUse:
		case deleteErr == nil && errorCode(bindErr) == apperror.FileNotReady:
		default:
			t.Errorf("invalid race outcome: bind=%v delete=%v", bindErr, deleteErr)
		}
	}
}

func errorCode(err error) string {
	var e *apperror.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
