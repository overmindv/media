package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/httpapi"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/service"
	"github.com/overmindv/media/internal/storage"
)

// main собирает зависимости и запускает внутренний HTTP API Media.
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("не удалось загрузить конфигурацию", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("не удалось создать PostgreSQL pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	objectStorage, err := storage.NewS3(ctx, cfg.S3)
	if err != nil {
		logger.Error("не удалось создать S3 adapter", "error", err)
		os.Exit(1)
	}
	media := service.New(repository.New(pool), objectStorage, cfg)
	server := &http.Server{
		Addr: cfg.HTTP.Address,
		Handler: httpapi.New(media, map[string]string{
			"gateway": cfg.GatewayToken,
			"users":   cfg.UsersToken,
		}, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       time.Minute,
	}
	go func() {
		logger.Info("Media API запущен", "address", cfg.HTTP.Address)
		if listenErr := server.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			logger.Error("Media API завершился с ошибкой", "error", listenErr)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("не удалось остановить Media API", "error", err)
	}
}
