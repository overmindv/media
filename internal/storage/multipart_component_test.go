//go:build component

package storage

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/service"
)

func TestMinIOMultipartRecovery(t *testing.T) {
	endpoint := os.Getenv("DELIVERY_TEST_S3_URL")
	if endpoint == "" {
		t.Skip("dedicated MinIO is required")
	}
	ctx := t.Context()
	s, err := NewS3(ctx, config.S3{
		Region: "us-east-1", Endpoint: endpoint, PublicEndpoint: endpoint, PathStyle: true,
		AccessKey: os.Getenv("DELIVERY_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("DELIVERY_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	const bucket = "media-quarantine"
	start := func() (string, string, []domain.CompletedPart) {
		key := "uploads/" + uuid.NewString()
		id, err := s.CreateMultipart(ctx, bucket, key, "application/octet-stream")
		if err != nil {
			t.Fatal(err)
		}
		part, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
			PartNumber: aws.Int32(1), Body: strings.NewReader("fixture"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return key, id, []domain.CompletedPart{{PartNumber: 1, ETag: aws.ToString(part.ETag)}}
	}
	key, id, parts := start()
	if err := s.CompleteMultipart(ctx, bucket, key, id, parts); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteMultipart(ctx, bucket, key, id, parts); !errors.Is(err, service.ErrMultipartMissing) {
		t.Error("repeat complete should report the consumed multipart ID", err)
	}
	if result, err := s.Head(ctx, bucket, key); err != nil || result.SizeBytes != 7 {
		t.Error("completed object was not recoverable", result.SizeBytes, err)
	}

	key, id, _ = start()
	for range 2 {
		if err := s.AbortMultipart(ctx, bucket, key, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
	}); !isMissingUpload(err) {
		t.Error("aborted multipart upload still exists", err)
	}
}
