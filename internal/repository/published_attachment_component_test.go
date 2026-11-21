//go:build component

package repository

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/httpapi"
	"github.com/overmindv/media/internal/service"
	"github.com/overmindv/media/internal/storage"
	"github.com/overmindv/media/internal/testutil"
)

func TestPublishedAttachmentHTTP(t *testing.T) {
	t.Parallel()

	repo := componentRepository(t)
	ctx := t.Context()
	owner := uuid.NewString()
	ref := domain.PublishedAttachmentRef{MaterialID: uuid.NewString(), RevisionID: uuid.NewString()}
	document := bindingFile(t, ctx, repo, owner, domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady)
	image := bindingFile(t, ctx, repo, owner, domain.PurposeContentImage, domain.VisibilityPrivate, domain.StatusReady)
	unbound := bindingFile(t, ctx, repo, owner, domain.PurposeAttachment, domain.VisibilityPrivate, domain.StatusReady)
	original, sanitized := "originals/verified/"+document, "variants/private/"+image+"/w320.webp"
	if _, err := repo.pool.Exec(ctx, `
		UPDATE blobs b SET object_key='originals/verified/'||f.id FROM files f WHERE b.id=f.blob_id AND f.id=ANY($1::uuid[])`,
		[]string{document, image}); err != nil {
		t.Fatal(err)
	}
	bind := domain.ResourceBinding{
		Scope: domain.BindingContentRevision, ResourceID: ref.MaterialID, EntityID: ref.RevisionID, Version: 1,
		BindingFiles: domain.BindingFiles{ActorUserID: owner, FileIDs: []string{document, image}},
	}
	if err := repo.ReplaceResourceBinding(ctx, bind); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DownloadTTL: 5 * time.Minute, S3: config.S3{
		Region: "us-east-1", Endpoint: "https://objects.invalid", PublicEndpoint: "https://objects.invalid",
		AccessKey: "fixture-access", SecretKey: "fixture-secret", PathStyle: true, MediaBucket: "media",
	}}
	objects, err := storage.NewS3(ctx, cfg.S3)
	if err != nil {
		t.Fatal(err)
	}
	router := http.NewServeMux()
	httpapi.Register(router, service.New(repo, objects, cfg), map[string]string{
		"content": "content-fixture", "entities": "entities-fixture", "gateway": "gateway-fixture",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(material, revision, file, token string, wantStatus int, wantKey string) {
		t.Helper()
		endpoint := "/v1/internal/content/materials/" + material + "/revisions/" + revision + "/files/" + file + "/download-url"
		req := httptest.NewRequest(http.MethodPost, endpoint, nil).WithContext(ctx)
		req.Header.Set("X-Media-Service-Token", token)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if out.Code != wantStatus {
			t.Error("download status", out.Code, "want", wantStatus)
		}
		if wantKey != "" {
			assertPublishedLink(t, out, wantKey)
		} else if strings.Contains(out.Body.String(), "X-Amz-Signature") {
			t.Error("refused response included a signed URL")
		}
	}
	const token = "content-fixture"
	for _, unauthorized := range []string{"", "entities-fixture", "gateway-fixture"} {
		request(ref.MaterialID, ref.RevisionID, document, unauthorized, http.StatusForbidden, "")
	}
	request(ref.MaterialID, ref.RevisionID, document, token, http.StatusOK, original)
	request(ref.MaterialID, ref.RevisionID, image, token, http.StatusNotFound, "")
	seedAttachmentVariants(t, repo, image, sanitized)
	request(ref.MaterialID, ref.RevisionID, image, token, http.StatusOK, sanitized)
	request(uuid.NewString(), ref.RevisionID, document, token, http.StatusNotFound, "")
	request(ref.MaterialID, uuid.NewString(), document, token, http.StatusNotFound, "")
	request(ref.MaterialID, ref.RevisionID, unbound, token, http.StatusNotFound, "")
	request(ref.MaterialID, ref.RevisionID, "not-a-uuid", token, http.StatusBadRequest, "")
	if _, err = repo.pool.Exec(ctx, "UPDATE blobs SET scan_verified=false WHERE id=(SELECT blob_id FROM files WHERE id=$1)", document); err != nil {
		t.Fatal(err)
	}
	request(ref.MaterialID, ref.RevisionID, document, token, http.StatusNotFound, "")
	bind.Version, bind.FileIDs = 2, []string{}
	if err = repo.ReplaceResourceBinding(ctx, bind); err != nil {
		t.Fatal(err)
	}
	request(ref.MaterialID, ref.RevisionID, image, token, http.StatusNotFound, "")
}

func seedAttachmentVariants(t *testing.T, repo *Postgres, image, sanitized string) {
	t.Helper()
	_, err := repo.pool.Exec(t.Context(), `
		INSERT INTO blob_variants (id,blob_id,name,format,width,height,size_bytes,checksum_sha256,object_key)
		SELECT gen_random_uuid(), f.blob_id, v.name, v.format, v.width, v.width, 16, f.checksum_sha256, v.key
		FROM files f CROSS JOIN (VALUES ('w128','webp',128,$2::text),('w320','webp',320,$3),
		     ('w1440','webp',1440,$4),('w768','png',768,$5)) v(name,format,width,key)
		WHERE f.id=$1`, image, "variants/private/"+image+"/w128.webp", sanitized,
		"originals/verified/"+image, "variants/private/"+image+"/unsafe.webp")
	if err != nil {
		t.Fatal(err)
	}
}

func assertPublishedLink(t *testing.T, out *httptest.ResponseRecorder, wantKey string) {
	t.Helper()
	const noStore = "private, no-store"
	var download domain.Download
	if err := json.Unmarshal(out.Body.Bytes(), &download); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(download.URL)
	if err != nil || link.Path != "/media/"+wantKey || link.Query().Get("X-Amz-Expires") != "30" ||
		link.Query().Get("response-content-disposition") != "attachment" ||
		link.Query().Get("response-content-type") != "application/octet-stream" ||
		link.Query().Get("response-cache-control") != noStore ||
		out.Header().Get("Cache-Control") != noStore || !download.ExpiresAt.After(time.Now()) {
		t.Error("expected a short-lived attachment link for the verified object")
	}
}

func BenchmarkPublishedAttachmentLookup(b *testing.B) {
	repo := New(testutil.Postgres(b), false)
	ids := seedPreviews(b, repo, 2000)
	ref := domain.PublishedAttachmentRef{MaterialID: uuid.NewString(), RevisionID: uuid.NewString(), FileID: ids[0]}
	_, err := repo.pool.Exec(b.Context(), `
		WITH fixtures AS (
			UPDATE files SET purpose='content_image' WHERE id=ANY($1::uuid[])
			RETURNING id, owner_user_id,
				CASE WHEN id=$2 THEN $3::uuid ELSE gen_random_uuid() END AS revision,
				CASE WHEN id=$2 THEN $4::uuid ELSE gen_random_uuid() END AS material
		), versions AS (
			INSERT INTO resource_binding_versions (scope,entity_id,resource_id,version,request_hash)
			SELECT 'content_revision',revision,material,1,decode(repeat('00',32),'hex') FROM fixtures
		)
		INSERT INTO file_bindings (id,file_id,service_name,entity_type,entity_id,created_by)
		SELECT gen_random_uuid(),id,'content','content_revision',revision,owner_user_id FROM fixtures`,
		ids, ref.FileID, ref.RevisionID, ref.MaterialID)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("owner_lookup_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			file, err := repo.GetFile(b.Context(), ref.FileID)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := repo.GetObjectKey(b.Context(), file.ID, "w128"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("published_binding_lookup", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := repo.PublishedAttachmentKey(b.Context(), ref); err != nil {
				b.Fatal(err)
			}
		}
	})
}
