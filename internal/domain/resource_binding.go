package domain

const (
	MaxResourceBindingFiles = 32

	BindingContentRevision BindingScope = "content_revision"
	BindingCatalogLogo     BindingScope = "catalog_logo"
	BindingUserAvatar      BindingScope = "user_avatar"
)

type BindingScope string

func (s BindingScope) ServiceName() string {
	switch s {
	case BindingContentRevision:
		return "content"
	case BindingCatalogLogo:
		return "entities"
	case BindingUserAvatar:
		return "users"
	default:
		return ""
	}
}

type BindingFiles struct {
	ActorUserID           string   `json:"actor_user_id"`
	FileIDs               []string `json:"file_ids"`
	InheritFromRevisionID string   `json:"inherit_from_revision_id,omitempty"`
}

type ResourceBinding struct {
	BindingFiles
	Scope      BindingScope
	ResourceID string
	EntityID   string
	Version    int64
}

type CatalogLogoBinding struct {
	Version int64   `json:"version"`
	FileID  *string `json:"file_id"`
}

type PublishedAttachmentRef struct {
	MaterialID string
	RevisionID string
	FileID     string
}
