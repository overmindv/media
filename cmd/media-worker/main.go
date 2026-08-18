package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/storage"
	"github.com/overmindv/media/internal/worker"
)

// main запускает отдельный процесс scan/processing worker.
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
	scanner := worker.NewClamAV(cfg.ClamAVAddress, 10*time.Minute)
	if err := scanner.Ping(); err != nil {
		logger.Error("ClamAV не готов", "error", err)
		os.Exit(1)
	}
	processor := worker.NewProcessor(repository.New(pool), objectStorage, scanner, cfg, logger)
	probeServer := startProbeServer(ctx, cfg.WorkerHTTPAddr, pool, objectStorage, cfg.ClamAVAddress, logger)
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := probeServer.Shutdown(shutdownContext); err != nil {
			logger.Error("не удалось остановить worker probe server", "error", err)
		}
	}()
	logger.Info("Media worker запущен")
	if err := processor.Run(ctx); err != nil {
		logger.Error("Media worker завершился с ошибкой", "error", err)
		os.Exit(1)
	}
}

// startProbeServer запускает health/readiness endpoint worker и возвращает сервер для graceful shutdown.
func startProbeServer(ctx context.Context, address string, pool *pgxpool.Pool, objectStorage *storage.S3, clamAVAddress string, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		probeContext, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := pool.Ping(probeContext); err != nil {
			http.Error(w, "postgres is not ready", http.StatusServiceUnavailable)

			return
		}
		if err := objectStorage.Ping(probeContext); err != nil {
			http.Error(w, "object storage is not ready", http.StatusServiceUnavailable)

			return
		}
		if err := worker.NewClamAV(clamAVAddress, 3*time.Second).Ping(); err != nil {
			http.Error(w, "clamav is not ready", http.StatusServiceUnavailable)

			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("worker probe server завершился с ошибкой", "error", fmt.Errorf("слушать %s: %w", address, err))
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			logger.Error("не удалось остановить worker probe server", "error", err)
		}
	}()

	return server
}
