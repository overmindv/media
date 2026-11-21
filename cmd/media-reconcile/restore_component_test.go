//go:build component

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/storage"
	"github.com/overmindv/media/internal/testutil"
)

func TestReportRestoredDatabaseAgainstOldBucket(t *testing.T) {
	endpoint := os.Getenv("DELIVERY_TEST_S3_URL")
	if endpoint == "" {
		t.Skip("dedicated MinIO is required")
	}
	ctx := t.Context()
	pool := testutil.Postgres(t)
	if _, err := pool.Exec(ctx, `
        WITH items AS (
            INSERT INTO blobs(id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key,scan_status,created_at,cleaned_at)
            VALUES(gen_random_uuid(),'missing',1,'private','image/png','originals/missing','clean',NOW()-interval '2 hours',NULL),
                  (gen_random_uuid(),'alive',1,'private','image/png','originals/alive','clean',NOW()-interval '2 hours',NULL),
                  (gen_random_uuid(),'restored',1,'private','image/png','originals/restored','clean',NOW()-interval '2 hours',NOW()-interval '1 hour')
            RETURNING id,checksum_sha256
        ), ready AS (
            INSERT INTO files(id,blob_id,owner_user_id,purpose,visibility,original_name,declared_content_type,size_bytes,checksum_sha256,status)
            SELECT gen_random_uuid(),id,gen_random_uuid(),'content_image','private','fixture','image/png',1,checksum_sha256,'ready'
              FROM items WHERE checksum_sha256<>'restored'
            RETURNING id
        ), scan AS (
            INSERT INTO media_jobs(id,file_id,kind,status) SELECT gen_random_uuid(),id,'scan_and_process','pending' FROM ready LIMIT 1
            RETURNING id
        )
        INSERT INTO media_job_objects(object_key,job_id,lease_token)
        SELECT 'originals/staged',id,gen_random_uuid() FROM scan`); err != nil {
		t.Fatal(err)
	}
	seed, err := storage.NewS3(ctx, config.S3{
		Endpoint: endpoint, PublicEndpoint: endpoint, Region: "us-east-1", PathStyle: true,
		AccessKey: os.Getenv("DELIVERY_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("DELIVERY_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := domain.PublicVariantPrefix + "restored/w128.webp"
	keys := []string{"originals/alive", "originals/orphan +%25", "originals/restored", "originals/staged", preview}
	for _, key := range keys {
		if err := seed.Put(ctx, "media", key, "image/png", strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	dsn, err := url.Parse(os.Getenv("COMPONENT_TEST_DSN"))
	if err != nil {
		t.Fatal("invalid fixture database URL")
	}
	args := dsn.Query()
	args.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = args.Encode()
	t.Setenv("MEDIA_RECONCILE_DATABASE_URL", dsn.String())
	t.Setenv("MEDIA_RECONCILE_S3_ENDPOINT", endpoint)

	access := os.Getenv("INVENTORY_TEST_S3_ACCESS_KEY")
	secret := os.Getenv("INVENTORY_TEST_S3_SECRET_KEY")
	if access == "" || secret == "" {
		t.Fatal("dedicated read-only S3 credentials are required for this test")
	}
	t.Setenv("MEDIA_RECONCILE_S3_ACCESS_KEY", access)
	t.Setenv("MEDIA_RECONCILE_S3_SECRET_KEY", secret)
	time.Sleep(1200 * time.Millisecond)

	var out bytes.Buffer
	if err := run(ctx, []string{"--scope", "originals", "--page-size", "1", "--grace", "1s"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var findings []string
	decoder := json.NewDecoder(&out)
	validSummary := false
	for {
		var row struct {
			Kind     string `json:"kind"`
			Key      string `json:"key"`
			Complete bool   `json:"complete"`
			Scanned  int    `json:"scanned"`
		}
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if row.Kind == "summary" {
			validSummary = row.Complete && row.Scanned == 5
		} else {
			findings = append(findings, row.Kind+":"+row.Key)
		}
	}
	want := []string{"missing:originals/missing", "orphan:originals/orphan +%25", "reappeared:originals/restored"}
	if !validSummary || !slices.Equal(findings, want) {
		t.Error("unexpected restore report", validSummary, findings)
	}

	out.Reset()
	if err := run(ctx, []string{"--scope", "verified-variants", "--page-size", "1", "--grace", "1s"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var variant struct{ Kind, Key string }
	if err := json.NewDecoder(&out).Decode(&variant); err != nil || variant.Kind != "orphan" || variant.Key != preview {
		t.Error("verified WebP did not appear in the report", variant, err)
	}

	for _, key := range keys {
		if _, err := seed.Head(ctx, "media", key); err != nil {
			t.Error("report changed the bucket", key, err)
		}
	}

	readOnlyCfg, err := storageConfig()
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := storage.NewS3(ctx, readOnlyCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Put(ctx, "media", "originals/forbidden", "image/png", strings.NewReader("x"), 1); err == nil {
		t.Error("inventory role can write to storage")
	}
	if _, err := readOnly.Head(ctx, "media", "originals/alive"); err == nil {
		t.Error("inventory role can read object contents")
	}
	if _, _, err := readOnly.InventoryPages("media", "")(ctx, "", 1); err == nil {
		t.Error("inventory role can list the entire bucket")
	}
}
