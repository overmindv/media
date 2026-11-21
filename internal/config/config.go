package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/overmindv/media/internal/render"
)

type Config struct {
	InternalToken        string
	GatewayToken         string
	UsersToken           string
	ContentToken         string
	EntitiesToken        string
	S3                   S3
	Limits               Limits
	UploadTTL            time.Duration
	DownloadTTL          time.Duration
	SessionTTL           time.Duration
	DeleteRetention      time.Duration
	WorkerPoll           time.Duration
	UserQuotaBytes       int64
	MaxMultipartSessions int
	ClamAVAddress        string
	RenderSocket         string
	AllowUnscannedLocal  bool
	EnableUploadCleanup  bool
	PublicBaseURL        string
	RequestLogPath       string
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
// HTTP и подключение к PostgreSQL владеет parker (HTTP_ADDR/DATABASE_URL).
func Load() (Config, error) {
	cfg := Config{
		InternalToken:        strings.TrimSpace(os.Getenv("MEDIA_INTERNAL_TOKEN")),
		GatewayToken:         strings.TrimSpace(os.Getenv("MEDIA_GATEWAY_TOKEN")),
		UsersToken:           strings.TrimSpace(os.Getenv("MEDIA_USERS_TOKEN")),
		ContentToken:         os.Getenv("MEDIA_CONTENT_TOKEN"),
		EntitiesToken:        os.Getenv("MEDIA_ENTITIES_TOKEN"),
		UploadTTL:            envDuration("MEDIA_UPLOAD_URL_TTL", 15*time.Minute),
		DownloadTTL:          envDuration("MEDIA_DOWNLOAD_URL_TTL", 5*time.Minute),
		SessionTTL:           envDuration("MEDIA_SESSION_TTL", 24*time.Hour),
		DeleteRetention:      envDuration("MEDIA_DELETE_RETENTION", 7*24*time.Hour),
		WorkerPoll:           envDuration("MEDIA_WORKER_POLL_INTERVAL", time.Second),
		UserQuotaBytes:       envInt64("MEDIA_USER_QUOTA_BYTES", 5<<30),
		MaxMultipartSessions: int(min(envInt64("MEDIA_MAX_MULTIPART_SESSIONS", 4), 1000)),
		ClamAVAddress:        env("MEDIA_CLAMAV_ADDR", "localhost:3310"),
		RenderSocket:         env("MEDIA_RENDER_SOCKET", render.SocketPath),
		PublicBaseURL:        strings.TrimRight(env("MEDIA_PUBLIC_BASE_URL", "http://localhost:9000/media"), "/"),
		RequestLogPath:       strings.TrimSpace(os.Getenv("MEDIA_REQUEST_LOG_PATH")),
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
			AvatarBytes:        min(envInt64("MEDIA_MAX_AVATAR_BYTES", render.MaxInputBytes), render.MaxInputBytes),
			MaxAvatarPixels:    min(envInt64("MEDIA_MAX_AVATAR_PIXELS", render.MaxPixels), render.MaxPixels),
			ImageBytes:         min(envInt64("MEDIA_MAX_IMAGE_BYTES", render.MaxInputBytes), render.MaxInputBytes),
			DocumentBytes:      envInt64("MEDIA_MAX_DOCUMENT_BYTES", 50<<20),
			ArchiveBytes:       envInt64("MEDIA_MAX_ARCHIVE_BYTES", 250<<20),
			MultipartThreshold: envInt64("MEDIA_MULTIPART_THRESHOLD", 100<<20),
			PartSize:           envInt64("MEDIA_MULTIPART_PART_SIZE", 16<<20),
			MaxImagePixels:     min(envInt64("MEDIA_MAX_IMAGE_PIXELS", render.MaxPixels), render.MaxPixels),
		},
	}
	if cfg.GatewayToken == "" {
		cfg.GatewayToken = cfg.InternalToken
	}
	if value, exists := os.LookupEnv("MEDIA_ALLOW_UNSCANNED_LOCAL"); exists {
		allow, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("invalid MEDIA_ALLOW_UNSCANNED_LOCAL")
		}
		if allow && os.Getenv("ENV") != "local" {
			return Config{}, fmt.Errorf("MEDIA_ALLOW_UNSCANNED_LOCAL requires ENV=local")
		}
		cfg.AllowUnscannedLocal = allow
	}
	if cfg.UsersToken == "" {
		cfg.UsersToken = cfg.InternalToken
	}
	if value, exists := os.LookupEnv("MEDIA_ENABLE_UPLOAD_CLEANUP"); exists {
		on, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("invalid MEDIA_ENABLE_UPLOAD_CLEANUP")
		}
		cfg.EnableUploadCleanup = on
		if on && cfg.S3.MediaBucket == cfg.S3.QuarantineBucket {
			return Config{}, fmt.Errorf("upload cleanup requires separate quarantine and media buckets")
		}
	}
	if cfg.GatewayToken == "" || cfg.UsersToken == "" {
		return Config{}, fmt.Errorf("MEDIA_GATEWAY_TOKEN и MEDIA_USERS_TOKEN обязательны")
	}
	usedTokens := map[string]bool{cfg.GatewayToken: true, cfg.UsersToken: true, cfg.InternalToken: true}
	for _, token := range []string{cfg.ContentToken, cfg.EntitiesToken} {
		if token == "" {
			continue
		}
		if len(token) < 32 || len(token) > 256 || usedTokens[token] ||
			strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
			return Config{}, fmt.Errorf("MEDIA_CONTENT_TOKEN and MEDIA_ENTITIES_TOKEN must be separate service tokens")
		}
		usedTokens[token] = true
	}
	if cfg.DownloadTTL > 15*time.Minute {
		return Config{}, fmt.Errorf("MEDIA_DOWNLOAD_URL_TTL must not exceed 15m")
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
