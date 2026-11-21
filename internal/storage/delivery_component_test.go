//go:build component

package storage

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
)

func TestMinIODelivery(t *testing.T) {
	endpoint := os.Getenv("DELIVERY_TEST_S3_URL")
	if endpoint == "" {
		t.Skip("dedicated MinIO with the public variant policy is required")
	}
	ctx := context.Background()
	s, err := NewS3(ctx, config.S3{
		Region: "us-east-1", Endpoint: endpoint, PublicEndpoint: endpoint, PathStyle: true,
		AccessKey: os.Getenv("DELIVERY_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("DELIVERY_TEST_S3_SECRET_KEY"),
		MediaBucket: "media",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	public := domain.PublicVariantPrefix + id + "/w128.webp"
	private := "variants/private/" + id + "/w128.webp"
	legacy := "variants/" + id + "/w128.webp"
	notWebP := domain.PublicVariantPrefix + id + "/fixture.txt"
	for _, key := range []string{public, private, legacy, notWebP} {
		if err := s.Put(ctx, "media", key, "image/webp", strings.NewReader("preview"), 7); err != nil {
			t.Fatal(err)
		}
	}
	if got, header := deliveryGet(t, endpoint+"/media/"+public); got != 200 || header.Get("Content-Type") != "image/webp" {
		t.Error("public preview response", got, header.Get("Content-Type"))
	}
	for _, key := range []string{private, legacy, notWebP, ""} {
		if status, _ := deliveryGet(t, endpoint+"/media/"+key); status != http.StatusForbidden {
			t.Error("anonymous access must be denied", key, status)
		}
	}
	for _, fixture := range []struct{ prefix, mime, data string }{
		{"public/verified/originals/", "text/html", "<p>delivery fixture</p>"},
		{"originals/", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"><circle r="10"/></svg>`},
	} {
		key := fixture.prefix + uuid.NewString()
		// Seed a legacy object without the upload headers.
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("media"), Key: aws.String(key),
			ContentType: aws.String(fixture.mime), Body: strings.NewReader(fixture.data),
		})
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := deliveryGet(t, endpoint+"/media/"+key); status != http.StatusForbidden {
			t.Error("anonymous original must be denied", status)
		}
		signed, err := s.PresignGet(ctx, "media", key, 10*time.Second, true)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(signed)
		if err != nil || parsed.Query().Get("X-Amz-Expires") != "10" {
			t.Fatal("unexpected signature expiry")
		}
		code, h := deliveryGet(t, signed)
		if code != 200 || h.Get("Content-Type") != "application/octet-stream" ||
			h.Get("Content-Disposition") != "attachment" || h.Get("Cache-Control") != privateCacheControl {
			t.Error("unsafe download response", code, h.Get("Content-Type"), h.Get("Content-Disposition"), h.Get("Cache-Control"))
		}
		short, err := s.PresignGet(ctx, "media", key, time.Second, true)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2200 * time.Millisecond)
		if code, _ := deliveryGet(t, short); code != http.StatusForbidden {
			t.Error("expired signature should be rejected", code)
		}
	}
}

func deliveryGet(t *testing.T, target string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal("invalid test URL")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("MinIO test request failed")
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode, response.Header
}
