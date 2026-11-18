package storage

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/overmindv/media/internal/config"
)

func TestS3HeadAndPublicPresign(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	const key = "materials/file.pdf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/media/"+key {
			t.Errorf("unexpected storage request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "16")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client, err := NewS3(t.Context(), config.S3{
		Region: "us-east-1", Endpoint: srv.URL, PublicEndpoint: "https://media.example.test",
		AccessKey: "fixture-access", SecretKey: "fixture-secret", MediaBucket: "media", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Head(t.Context(), "media", key)
	if err != nil || info.SizeBytes != 16 || info.ContentType != "application/pdf" {
		t.Errorf("HEAD returned %+v: %v", info, err)
	}
	signed, err := client.PresignGet(t.Context(), "media", key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil || parsed.Host != "media.example.test" || parsed.Path != "/media/"+key ||
		parsed.Query().Get("X-Amz-Expires") != "60" || parsed.Query().Get("X-Amz-Signature") == "" {
		t.Error("signed read did not use the public endpoint with the expected expiry")
	}
	_, fields, err := client.PresignPost(t.Context(), "media", key, "application/pdf", 16, time.Minute)
	if err != nil || fields["Content-Type"] != "application/pdf" || len(fields) == 0 {
		t.Errorf("presigned POST omitted the content type: %v", err)
	}
}
