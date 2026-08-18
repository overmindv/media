package storage

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/service"
)

type S3 struct {
	client        *s3.Client
	presigner     *s3.PresignClient
	publicPresign *s3.PresignClient
	mediaBucket   string
}

// NewS3 создаёт S3-compatible adapter с отдельным browser-visible endpoint.
func NewS3(ctx context.Context, cfg config.S3) (*S3, error) {
	base, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("загрузить AWS config: %w", err)
	}
	client := s3.NewFromConfig(base, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(cfg.Endpoint)
		options.UsePathStyle = cfg.PathStyle
	})
	publicClient := s3.NewFromConfig(base, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(cfg.PublicEndpoint)
		options.UsePathStyle = cfg.PathStyle
	})

	return &S3{
		client:        client,
		presigner:     s3.NewPresignClient(client),
		publicPresign: s3.NewPresignClient(publicClient),
		mediaBucket:   cfg.MediaBucket,
	}, nil
}

// Ping проверяет доступность media bucket.
func (s *S3) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.mediaBucket)})
	if err != nil {
		return fmt.Errorf("head media bucket: %w", err)
	}

	return nil
}

// PresignPost создаёт browser POST policy с точным размером и content type.
func (s *S3) PresignPost(ctx context.Context, bucket, key, contentType string, size int64, ttl time.Duration) (string, map[string]string, error) {
	result, err := s.publicPresign.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, func(options *s3.PresignPostOptions) {
		options.Expires = ttl
		options.Conditions = append(options.Conditions,
			[]any{"content-length-range", size, size},
			map[string]string{"Content-Type": contentType},
		)
	})
	if err != nil {
		return "", nil, fmt.Errorf("presign POST object: %w", err)
	}

	return result.URL, result.Values, nil
}

// CreateMultipart начинает multipart upload.
func (s *S3) CreateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("create multipart upload: %w", err)
	}

	return aws.ToString(result.UploadId), nil
}

// PresignPart создаёт URL для одной multipart-части.
func (s *S3) PresignPart(ctx context.Context, bucket, key, uploadID string, part int32, ttl time.Duration) (string, error) {
	result, err := s.publicPresign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(part),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign upload part: %w", err)
	}

	return result.URL, nil
}

// CompleteMultipart собирает загруженные части в объект.
func (s *S3) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []domain.CompletedPart) error {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, part := range parts {
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(part.ETag),
			PartNumber: aws.Int32(part.PartNumber),
		})
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	if err != nil {
		return fmt.Errorf("complete multipart upload: %w", err)
	}

	return nil
}

// Head возвращает проверяемые метаданные объекта.
func (s *S3) Head(ctx context.Context, bucket, key string) (service.ObjectInfo, error) {
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return service.ObjectInfo{}, fmt.Errorf("head object: %w", err)
	}
	checksum := ""
	if result.ChecksumSHA256 != nil {
		bytes, decodeErr := base64.StdEncoding.DecodeString(aws.ToString(result.ChecksumSHA256))
		if decodeErr == nil {
			checksum = fmt.Sprintf("%x", bytes)
		}
	}

	return service.ObjectInfo{
		SizeBytes:   aws.ToInt64(result.ContentLength),
		Checksum:    checksum,
		ContentType: strings.TrimSpace(aws.ToString(result.ContentType)),
	}, nil
}

// PresignGet создаёт короткий browser-visible GET URL.
func (s *S3) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	result, err := s.publicPresign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get object: %w", err)
	}

	return result.URL, nil
}

// Get открывает поток чтения объекта; закрытие потока остаётся вызывающей стороне.
func (s *S3) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}

	return result.Body, nil
}

// Put потоково записывает обработанный объект с явным MIME и размером.
func (s *S3) Put(ctx context.Context, bucket, key, contentType string, body io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
		CacheControl:  aws.String("public,max-age=31536000,immutable"),
	})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}

	return nil
}

// Copy копирует объект между quarantine и delivery bucket без проксирования байтов приложением.
func (s *S3) Copy(ctx context.Context, sourceBucket, sourceKey, targetBucket, targetKey, contentType string) error {
	source := url.PathEscape(sourceBucket + "/" + sourceKey)
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(targetBucket),
		Key:               aws.String(targetKey),
		CopySource:        aws.String(source),
		ContentType:       aws.String(contentType),
		MetadataDirective: types.MetadataDirectiveReplace,
	})
	if err != nil {
		return fmt.Errorf("copy object: %w", err)
	}

	return nil
}

// Delete удаляет физический объект из storage.
func (s *S3) Delete(ctx context.Context, bucket, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}

	return nil
}
