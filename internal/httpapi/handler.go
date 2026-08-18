package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/service"
)

const (
	requestIDHeader    = "X-Request-ID"
	userIDHeader       = "X-User-ID"
	userRolesHeader    = "X-User-Roles"
	serviceTokenHeader = "X-Media-Service-Token"
)

type Handler struct {
	service       *service.Service
	logger        *slog.Logger
	serviceTokens map[string]string
	limiter       *uploadLimiter
	requests      atomic.Uint64
	errors        atomic.Uint64
}

// New создаёт внутренний HTTP API Media с auth, rate limit и observability middleware.
func New(media *service.Service, serviceTokens map[string]string, logger *slog.Logger) http.Handler {
	handler := &Handler{
		service:       media,
		logger:        logger,
		serviceTokens: serviceTokens,
		limiter:       newUploadLimiter(30, time.Minute),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handler.health)
	mux.HandleFunc("GET /ready", handler.ready)
	mux.HandleFunc("GET /metrics", handler.metrics)
	mux.Handle("POST /v1/uploads", handler.gatewayOnly(http.HandlerFunc(handler.createUpload)))
	mux.Handle("POST /v1/uploads/{id}/parts", handler.gatewayOnly(http.HandlerFunc(handler.createUploadParts)))
	mux.Handle("POST /v1/uploads/{id}/complete", handler.gatewayOnly(http.HandlerFunc(handler.completeUpload)))
	mux.Handle("GET /v1/files", handler.gatewayOnly(http.HandlerFunc(handler.listFiles)))
	mux.Handle("GET /v1/files/{id}", handler.gatewayOnly(http.HandlerFunc(handler.getFile)))
	mux.Handle("POST /v1/files/{id}/download-url", handler.gatewayOnly(http.HandlerFunc(handler.downloadURL)))
	mux.Handle("DELETE /v1/files/{id}", handler.gatewayOnly(http.HandlerFunc(handler.deleteFile)))
	mux.HandleFunc("POST /v1/internal/public-files/resolve", handler.resolvePublicFiles)
	mux.HandleFunc("GET /v1/internal/users/{user_id}/avatar-files/{file_id}/validate", handler.validateAvatar)
	mux.HandleFunc("PUT /v1/internal/users/{id}/avatar-binding", handler.replaceAvatarBinding)

	return handler.requestID(handler.logging(handler.recover(handler.internalAuth(mux))))
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Ready(r.Context()); err != nil {
		h.writeError(w, fmt.Errorf("readiness: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w, "media_http_requests_total %d\nmedia_http_errors_total %d\n", h.requests.Load(), h.errors.Load())
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request) {
	actor := actorFromRequest(r)
	if !h.limiter.Allow(actor.UserID) {
		h.writeError(w, apperror.New(apperror.PermissionDenied, "слишком много upload-запросов", http.StatusTooManyRequests))
		return
	}
	var input domain.CreateUploadInput
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.CreateUpload(r.Context(), input, actor)
	h.respond(w, http.StatusCreated, result, err)
}

func (h *Handler) createUploadParts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PartNumbers []int32 `json:"part_numbers"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.CreateUploadParts(r.Context(), r.PathValue("id"), input.PartNumbers, actorFromRequest(r))
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Parts []domain.CompletedPart `json:"parts"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.CompleteUpload(r.Context(), r.PathValue("id"), input.Parts, actorFromRequest(r))
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) getFile(w http.ResponseWriter, r *http.Request) {
	result, err := h.service.GetFile(r.Context(), r.PathValue("id"), actorFromRequest(r))
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) listFiles(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	result, err := h.service.ListFiles(r.Context(), actorFromRequest(r), r.URL.Query().Get("status"), limit, offset)
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) downloadURL(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Variant string `json:"variant"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.DownloadURL(r.Context(), r.PathValue("id"), input.Variant, actorFromRequest(r))
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	err := h.service.DeleteFile(r.Context(), r.PathValue("id"), actorFromRequest(r))
	h.respond(w, http.StatusOK, map[string]bool{"deleted": err == nil}, err)
}

func (h *Handler) resolvePublicFiles(w http.ResponseWriter, r *http.Request) {
	if serviceFromRequest(r) != "gateway" {
		h.writeError(w, apperror.New(apperror.PermissionDenied, "endpoint доступен только gateway", http.StatusForbidden))

		return
	}
	var input struct {
		FileIDs  []string `json:"file_ids"`
		Variants []string `json:"variants"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := h.service.ResolvePublicFiles(r.Context(), input.FileIDs, input.Variants)
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) validateAvatar(w http.ResponseWriter, r *http.Request) {
	if serviceFromRequest(r) != "users" {
		h.writeError(w, apperror.New(apperror.PermissionDenied, "endpoint доступен только users", http.StatusForbidden))

		return
	}
	err := h.service.ValidateAvatar(r.Context(), r.PathValue("user_id"), r.PathValue("file_id"))
	h.respond(w, http.StatusOK, map[string]bool{"valid": err == nil}, err)
}

func (h *Handler) replaceAvatarBinding(w http.ResponseWriter, r *http.Request) {
	if serviceFromRequest(r) != "users" {
		h.writeError(w, apperror.New(apperror.PermissionDenied, "endpoint доступен только users", http.StatusForbidden))

		return
	}
	var input struct {
		FileID *string `json:"file_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	err := h.service.ReplaceAvatarBinding(r.Context(), r.PathValue("id"), input.FileID)
	h.respond(w, http.StatusOK, map[string]bool{"updated": err == nil}, err)
}

func (h *Handler) respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, status, value)
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	h.errors.Add(1)
	var public *apperror.Error
	if errors.As(err, &public) {
		writeJSON(w, public.Status, public)
		return
	}
	h.logger.Error("внутренняя ошибка Media", "error", err)
	writeJSON(w, http.StatusInternalServerError, apperror.New(apperror.InternalError, "внутренняя ошибка", http.StatusInternalServerError))
}

func (h *Handler) internalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get(serviceTokenHeader)
		serviceName := ""
		for name, expected := range h.serviceTokens {
			if len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
				serviceName = name
				break
			}
		}
		if serviceName == "" {
			h.writeError(w, apperror.New(apperror.PermissionDenied, "внутренний вызов не авторизован", http.StatusForbidden))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), serviceContextKey{}, serviceName)))
	})
}

type serviceContextKey struct{}

func serviceFromRequest(r *http.Request) string {
	value, _ := r.Context().Value(serviceContextKey{}).(string)

	return value
}

// gatewayOnly ограничивает пользовательские use cases доверенным gateway token.
func (h *Handler) gatewayOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serviceFromRequest(r) != "gateway" {
			h.writeError(w, apperror.New(apperror.PermissionDenied, "endpoint доступен только gateway", http.StatusForbidden))

			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if requestID == "" {
			requestID = fmt.Sprintf("media-%d", time.Now().UnixNano())
		}
		r.Header.Set(requestIDHeader, requestID)
		w.Header().Set(requestIDHeader, requestID)
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		started := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/health" && r.URL.Path != "/ready" && r.URL.Path != "/metrics" {
			h.logger.Info("Media HTTP request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds(), "request_id", r.Header.Get(requestIDHeader))
		}
	})
}

func (h *Handler) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				h.logger.Error("Media panic recovered", "panic", recovered, "request_id", r.Header.Get(requestIDHeader))
				h.writeError(w, fmt.Errorf("panic: %v", recovered))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func actorFromRequest(r *http.Request) domain.Actor {
	roles := make([]string, 0)
	for _, role := range strings.Split(r.Header.Get(userRolesHeader), ",") {
		if role = strings.TrimSpace(role); role != "" {
			roles = append(roles, role)
		}
	}

	return domain.Actor{UserID: strings.TrimSpace(r.Header.Get(userIDHeader)), Roles: roles}
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, apperror.New(apperror.ValidationError, "некорректный JSON", http.StatusBadRequest))
		return false
	}

	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type uploadWindow struct {
	started time.Time
	count   int
}

type uploadLimiter struct {
	mutex  sync.Mutex
	items  map[string]uploadWindow
	limit  int
	window time.Duration
}

func newUploadLimiter(limit int, window time.Duration) *uploadLimiter {
	return &uploadLimiter{items: make(map[string]uploadWindow), limit: limit, window: window}
}

// Allow применяет process-local защиту от всплесков; глобальные лимиты остаются на gateway.
func (l *uploadLimiter) Allow(key string) bool {
	if key == "" {
		return true
	}
	l.mutex.Lock()
	defer l.mutex.Unlock()
	now := time.Now()
	item := l.items[key]
	if item.started.IsZero() || now.Sub(item.started) >= l.window {
		l.items[key] = uploadWindow{started: now, count: 1}
		return true
	}
	if item.count >= l.limit {
		return false
	}
	item.count++
	l.items[key] = item

	return true
}
