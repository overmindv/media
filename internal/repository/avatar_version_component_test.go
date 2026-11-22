//go:build component

package repository

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/httpapi"
	"github.com/overmindv/media/internal/service"
)

func TestAvatarVersionOrder(t *testing.T) {
	ctx := context.Background()
	repo := componentRepository(t)
	owner := uuid.NewString()
	files := make([]string, 3)
	for i := range files {
		files[i] = bindingFile(t, ctx, repo, owner, domain.PurposeAvatar, domain.VisibilityPublic, domain.StatusReady)
		_, err := repo.pool.Exec(ctx, `INSERT INTO blob_variants (id,blob_id,name,format,object_key,width,height,size_bytes,checksum_sha256) SELECT gen_random_uuid(),blob_id,'128','webp',$2,128,128,16,'fixture' FROM files WHERE id=$1`, files[i], domain.PublicVariantPrefix+files[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	router := http.NewServeMux()
	httpapi.Register(router, service.New(repo, nil, config.Config{}), map[string]string{"users": "users-test", "gateway": "gateway-test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	send := func(route, token, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, route, strings.NewReader(body))
		req.Header.Set("X-Media-Service-Token", token)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("got %d %s want %d", res.Code, res.Body.String(), want)
		}
	}
	path := "/v1/internal/users/" + owner + "/avatar-binding/versioned"
	body := func(v int, file string) string {
		if file == "" {
			return fmt.Sprintf(`{"version":%d,"file_id":null}`, v)
		}
		return fmt.Sprintf(`{"version":%d,"file_id":%q}`, v, file)
	}
	send(path, "gateway-test", body(1, files[0]), 403)
	for _, i := range []int{2, 0, 1, 2} {
		send(path, "users-test", body(i+1, files[i]), 204)
	}
	var current string
	if err := repo.pool.QueryRow(ctx, `SELECT file_id FROM file_bindings WHERE entity_id=$1 AND deleted_at IS NULL`, owner).Scan(&current); err != nil || current != files[2] {
		t.Fatalf("current %s err %v", current, err)
	}
	send(path, "users-test", body(3, files[0]), 409)
	send("/v1/internal/users/"+owner+"/avatar-binding", "users-test", fmt.Sprintf(`{"file_id":%q}`, files[0]), 409)
	send(path, "users-test", body(4, ""), 204)
	send(path, "users-test", body(2, files[1]), 204)
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM file_bindings WHERE entity_id=$1 AND deleted_at IS NULL`, owner).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bindings %d err %v", count, err)
	}
}
