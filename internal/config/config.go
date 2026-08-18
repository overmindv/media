package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTP            HTTP
	DatabaseURL     string
	InternalToken   string
	GatewayToken    string
	UsersToken      string
	S3              S3
	Limits          Limits
	UploadTTL       time.Duration
	DownloadTTL     time.Duration
	SessionTTL      time.Duration
	DeleteRetention time.Duration
	WorkerPoll      time.Duration
	WorkerHTTPAddr  string
	UserQuotaBytes  int64
	ClamAVAddress   string
	PublicBaseURL   string
	RequestLogPath  string
}

type HTTP struct {
	Address      string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

type S3 struct {
	Region           string
	Endpoint         string
	PublicEndpoint   string
	AccessKey        string
	SecretKey        string
	QuarantineBucket string
	MediaBucket      string
	PathStyle        bool
}

type Limits struct {
	AvatarBytes        int64
	MaxAvatarPixels    int64
	ImageBytes         int64
	DocumentBytes      int64
	ArchiveBytes       int64
	MultipartThreshold int64
	PartSize           int64
	MaxImagePixels     int64
}

// Load читает и валидирует runtime-конфигурацию Media.
func Load() (Config, error) {
	cfg := Config{
		HTTP: HTTP{
			Address:      env("MEDIA_HTTP_ADDR", ":8080"),
			ReadTimeout:  envDuration("MEDIA_READ_TIMEOUT", 10*time.Second),
			WriteTimeout: envDuration("MEDIA_WRITE_TIMEOUT", 20*time.Second),
		},
		DatabaseURL:     env("MEDIA_DATABASE_URL", "postgres://media:media@localhost:5435/media?sslmode=disable"),
		InternalToken:   strings.TrimSpace(os.Getenv("MEDIA_INTERNAL_TOKEN")),
		GatewayToken:    strings.TrimSpace(os.Getenv("MEDIA_GATEWAY_TOKEN")),
		UsersToken:      strings.TrimSpace(os.Getenv("MEDIA_USERS_TOKEN")),
		UploadTTL:       envDuration("MEDIA_UPLOAD_URL_TTL", 15*time.Minute),
		DownloadTTL:     envDuration("MEDIA_DOWNLOAD_URL_TTL", 5*time.Minute),
		SessionTTL:      envDuration("MEDIA_SESSION_TTL", 24*time.Hour),
		DeleteRetention: envDuration("MEDIA_DELETE_RETENTION", 7*24*time.Hour),
		WorkerPoll:      envDuration("MEDIA_WORKER_POLL_INTERVAL", time.Second),
		WorkerHTTPAddr:  env("MEDIA_WORKER_HTTP_ADDR", ":8081"),
		UserQuotaBytes:  envInt64("MEDIA_USER_QUOTA_BYTES", 5<<30),
		ClamAVAddress:   env("MEDIA_CLAMAV_ADDR", "localhost:3310"),
		PublicBaseURL:   strings.TrimRight(env("MEDIA_PUBLIC_BASE_URL", "http://localhost:9000/media"), "/"),
		RequestLogPath:  strings.TrimSpace(os.Getenv("MEDIA_REQUEST_LOG_PATH")),
		S3: S3{
			Region:           env("MEDIA_S3_REGION", "us-east-1"),
			Endpoint:         env("MEDIA_S3_ENDPOINT", "http://localhost:9000"),
			PublicEndpoint:   env("MEDIA_S3_PUBLIC_ENDPOINT", "http://localhost:9000"),
			AccessKey:        env("MEDIA_S3_ACCESS_KEY", "minio"),
			SecretKey:        env("MEDIA_S3_SECRET_KEY", "minio-development"),
			QuarantineBucket: env("MEDIA_S3_QUARANTINE_BUCKET", "media-quarantine"),
			MediaBucket:      env("MEDIA_S3_MEDIA_BUCKET", "media"),
			PathStyle:        envBool("MEDIA_S3_PATH_STYLE", true),
		},
		Limits: Limits{
			AvatarBytes:        envInt64("MEDIA_MAX_AVATAR_BYTES", 5<<20),
			MaxAvatarPixels:    envInt64("MEDIA_MAX_AVATAR_PIXELS", 12_000_000),
			ImageBytes:         envInt64("MEDIA_MAX_IMAGE_BYTES", 20<<20),
			DocumentBytes:      envInt64("MEDIA_MAX_DOCUMENT_BYTES", 50<<20),
			ArchiveBytes:       envInt64("MEDIA_MAX_ARCHIVE_BYTES", 250<<20),
			MultipartThreshold: envInt64("MEDIA_MULTIPART_THRESHOLD", 100<<20),
			PartSize:           envInt64("MEDIA_MULTIPART_PART_SIZE", 16<<20),
			MaxImagePixels:     envInt64("MEDIA_MAX_IMAGE_PIXELS", 40_000_000),
		},
	}
	if cfg.GatewayToken == "" {
		cfg.GatewayToken = cfg.InternalToken
	}
	if cfg.UsersToken == "" {
		cfg.UsersToken = cfg.InternalToken
	}
	if cfg.DatabaseURL == "" || cfg.GatewayToken == "" || cfg.UsersToken == "" {
		return Config{}, fmt.Errorf("MEDIA_DATABASE_URL, MEDIA_GATEWAY_TOKEN и MEDIA_USERS_TOKEN обязательны")
	}
	if cfg.S3.Endpoint == "" || cfg.S3.PublicEndpoint == "" || cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
		return Config{}, fmt.Errorf("S3 endpoint и credentials обязательны")
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(env(key, fallback.String()))
	if err != nil || value <= 0 {
		return fallback
	}

	return value
}

func envInt64(key string, fallback int64) int64 {
	value, err := strconv.ParseInt(env(key, strconv.FormatInt(fallback, 10)), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}

	return value
}

func envBool(key string, fallback bool) bool {
	value, err := strconv.ParseBool(env(key, strconv.FormatBool(fallback)))
	if err != nil {
		return fallback
	}

	return value
}
