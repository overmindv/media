package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

func (h *Handler) registerResourceBindings(router Router) {
	router.Handle("PUT /v1/internal/users/{id}/avatar-binding/versioned", h.bindingOnly(domain.BindingUserAvatar, h.replaceVersionedAvatar))
	content, catalog := domain.BindingContentRevision, domain.BindingCatalogLogo
	router.Handle("POST /v1/internal/content/binding-files/check", h.bindingOnly(content, h.checkBindingFiles))
	router.Handle("PUT /v1/internal/content/materials/{material_id}/revisions/{revision_id}/bindings", h.bindingOnly(content, h.replaceResourceBinding))
	router.Handle("POST /v1/internal/content/materials/{material_id}/revisions/{revision_id}/files/{file_id}/download-url",
		h.bindingOnly(content, h.downloadPublishedAttachment))
	router.Handle("POST /v1/internal/entities/binding-files/check", h.bindingOnly(catalog, h.checkBindingFiles))
	router.Handle("PUT /v1/internal/entities/logos/{entity_id}/bindings", h.bindingOnly(catalog, h.replaceResourceBinding))
	router.Handle("GET /v1/internal/entities/logos/{entity_id}/bindings", h.bindingOnly(catalog, h.catalogLogoBinding))
}

func (h *Handler) catalogLogoBinding(w http.ResponseWriter, r *http.Request, _ domain.BindingScope) {
	state, err := h.service.CatalogLogoBinding(r.Context(), r.PathValue("entity_id"))
	w.Header().Set("Cache-Control", "private, no-store")
	h.respond(w, http.StatusOK, state, err)
}

func (h *Handler) replaceVersionedAvatar(w http.ResponseWriter, r *http.Request, scope domain.BindingScope) {
	var body struct {
		FileID  *string `json:"file_id"`
		Version int64   `json:"version"`
	}
	if !h.decodeBinding(w, r, &body) {
		return
	}
	ids := []string{}
	if body.FileID != nil {
		ids = append(ids, *body.FileID)
	}
	id := r.PathValue("id")
	h.bindingResponse(w, h.service.ReplaceResourceBinding(r.Context(), domain.ResourceBinding{
		Scope: scope, ResourceID: id, EntityID: id, Version: body.Version,
		BindingFiles: domain.BindingFiles{ActorUserID: id, FileIDs: ids},
	}))
}

func (h *Handler) downloadPublishedAttachment(w http.ResponseWriter, r *http.Request, _ domain.BindingScope) {
	ref := domain.PublishedAttachmentRef{
		MaterialID: r.PathValue("material_id"), RevisionID: r.PathValue("revision_id"), FileID: r.PathValue("file_id"),
	}
	result, err := h.service.DownloadPublishedAttachment(r.Context(), ref)
	w.Header().Set("Cache-Control", "private, no-store")
	h.respond(w, http.StatusOK, result, err)
}

func (h *Handler) bindingOnly(scope domain.BindingScope, next func(http.ResponseWriter, *http.Request, domain.BindingScope)) http.Handler {
	return h.internalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serviceFromRequest(r) != scope.ServiceName() {
			h.writeError(w, apperror.New(apperror.PermissionDenied, "чужая область привязок", http.StatusForbidden))

			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx), scope)
	}))
}

func (h *Handler) checkBindingFiles(w http.ResponseWriter, r *http.Request, scope domain.BindingScope) {
	var body struct {
		domain.BindingFiles
		MaterialID string `json:"material_id,omitempty"`
	}
	if !h.decodeBinding(w, r, &body) {
		return
	}
	input := domain.ResourceBinding{Scope: scope, ResourceID: body.MaterialID, BindingFiles: body.BindingFiles}
	h.bindingResponse(w, h.service.CheckResourceBindingFiles(r.Context(), input))
}

func (h *Handler) replaceResourceBinding(w http.ResponseWriter, r *http.Request, scope domain.BindingScope) {
	var body struct {
		domain.BindingFiles
		Version int64 `json:"version"`
	}
	if !h.decodeBinding(w, r, &body) {
		return
	}
	input := domain.ResourceBinding{Scope: scope, BindingFiles: body.BindingFiles, Version: body.Version}
	if scope == domain.BindingContentRevision {
		input.ResourceID, input.EntityID = r.PathValue("material_id"), r.PathValue("revision_id")
	} else {
		input.EntityID = r.PathValue("entity_id")
		input.ResourceID = input.EntityID
	}
	h.bindingResponse(w, h.service.ReplaceResourceBinding(r.Context(), input))
}

func (h *Handler) decodeBinding(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		h.writeError(w, apperror.New(apperror.ValidationError, "некорректный JSON", http.StatusBadRequest))

		return false
	}

	return true
}

func (h *Handler) bindingResponse(w http.ResponseWriter, err error) {
	if err != nil {
		h.writeError(w, err)

		return
	}
	w.WriteHeader(http.StatusNoContent)
}
