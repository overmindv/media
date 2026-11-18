package service

import (
	"context"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/domain"
)

func (s *Service) CheckResourceBindingFiles(ctx context.Context, binding domain.ResourceBinding) error {
	normalized, err := normalizeResourceBinding(binding, false)
	if err != nil {
		return err
	}
	return s.repository.CheckResourceBindingFiles(ctx, normalized)
}

func (s *Service) ReplaceResourceBinding(ctx context.Context, binding domain.ResourceBinding) error {
	normalized, err := normalizeResourceBinding(binding, true)
	if err != nil {
		return err
	}
	return s.repository.ReplaceResourceBinding(ctx, normalized)
}

func normalizeResourceBinding(binding domain.ResourceBinding, replace bool) (domain.ResourceBinding, error) {
	invalid := apperror.New(apperror.ValidationError, "некорректные параметры привязки", http.StatusBadRequest)
	if binding.Scope.ServiceName() == "" || binding.FileIDs == nil || len(binding.FileIDs) > domain.MaxResourceBindingFiles {
		return domain.ResourceBinding{}, invalid
	}
	if binding.Scope == domain.BindingCatalogLogo && (len(binding.FileIDs) > 1 || binding.InheritFromRevisionID != "") {
		return domain.ResourceBinding{}, invalid
	}
	if !replace && len(binding.FileIDs) == 0 {
		return domain.ResourceBinding{}, invalid
	}
	actor, ok := bindingID(binding.ActorUserID)
	if !ok {
		return domain.ResourceBinding{}, invalid
	}
	binding.ActorUserID = actor
	if replace || binding.ResourceID != "" || binding.InheritFromRevisionID != "" {
		binding.ResourceID, ok = bindingID(binding.ResourceID)
		if !ok {
			return domain.ResourceBinding{}, invalid
		}
	}
	if replace {
		binding.EntityID, ok = bindingID(binding.EntityID)
		if !ok || binding.Version < 1 || (binding.Scope == domain.BindingCatalogLogo && binding.EntityID != binding.ResourceID) {
			return domain.ResourceBinding{}, invalid
		}
	}
	if binding.InheritFromRevisionID != "" {
		binding.InheritFromRevisionID, ok = bindingID(binding.InheritFromRevisionID)
		if !ok || binding.InheritFromRevisionID == binding.EntityID || len(binding.FileIDs) == 0 {
			return domain.ResourceBinding{}, invalid
		}
	}
	ids := make([]string, len(binding.FileIDs))
	for i, value := range binding.FileIDs {
		ids[i], ok = bindingID(value)
		if !ok {
			return domain.ResourceBinding{}, invalid
		}
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != len(ids) {
		return domain.ResourceBinding{}, invalid
	}
	binding.FileIDs = ids
	return binding, nil
}

func bindingID(value string) (string, bool) {
	id, err := uuid.Parse(value)
	return id.String(), len(value) == 36 && err == nil && id != uuid.Nil
}
