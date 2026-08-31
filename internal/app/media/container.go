// Package media связывает паркер-инфраструктуру с бизнес-логикой Media.
package media

import (
	"context"
	"fmt"
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

	repo := repository.New(pool)
	media := service.New(repo, objectStorage, cfg)

	httpapi.Register(app.HTTP(), media, map[string]string{
		"gateway": cfg.GatewayToken,
		"users":   cfg.UsersToken,
	}, app.Logger())

	scanner := worker.NewClamAV(cfg.ClamAVAddress, 10*time.Minute)
	if !scanner.Available(3 * time.Second) {
		// Антивирус необязателен: worker работает и без clamd, сканирование пропускается.
		app.Logger().Warn("ClamAV недоступен — worker работает без антивирусной проверки", "address", cfg.ClamAVAddress)
	}
	processor := worker.NewProcessor(repo, objectStorage, scanner, cfg, app.Logger())

	app.AddRunnable("media-worker", processor.Run)

	return nil
}
