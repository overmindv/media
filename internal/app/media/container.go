// Package media связывает паркер-инфраструктуру с бизнес-логикой Media.
package media

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/overmindv/parker"

	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/httpapi"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/service"
	"github.com/overmindv/media/internal/storage"
	"github.com/overmindv/media/internal/worker"
)

// Build регистрирует зависимости Media на каркас parker.
// PostgreSQL, HTTP/health/metrics/logging предоставляет parker; здесь остаётся
// S3-адаптер, внутренний HTTP API и scan/processing worker как Runnable.
func Build(app *parker.App) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load media config: %w", err)
	}

	pool, err := app.Postgres()
	if err != nil {
		return fmt.Errorf("media postgres: %w", err)
	}

	objectStorage, err := storage.NewS3(context.Background(), cfg.S3)
	if err != nil {
		return fmt.Errorf("create media S3 adapter: %w", err)
	}
	app.AddHealthCheck("s3", parker.HealthCheckFunc(objectStorage.Ping))

	repo := repository.New(pool, cfg.AllowUnscannedLocal)
	media := service.New(repo, objectStorage, cfg)
	app.HTTP().HandleFunc("GET /metrics/scan", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		count, err := repo.ScanBacklog(ctx)
		if err != nil {
			http.Error(w, "scan backlog unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprintf(w, "# HELP media_scan_backlog_jobs Pending and running scan jobs.\n"+
			"# TYPE media_scan_backlog_jobs gauge\nmedia_scan_backlog_jobs %d\n", count)
	})

	httpapi.Register(app.HTTP(), media, map[string]string{
		"gateway": cfg.GatewayToken,
		"users":   cfg.UsersToken,
	}, app.Logger())

	scanner := worker.NewClamAV(cfg.ClamAVAddress, 10*time.Minute)
	if cfg.AllowUnscannedLocal {
		app.Logger().Warn("Media scan bypass enabled for local development")
	}
	processor := worker.NewProcessor(repo, objectStorage, scanner, cfg, app.Logger())

	app.AddRunnable("media-worker", processor.Run)

	return nil
}
