package domain

const (
	MaxResourceBindingFiles = 32

	BindingContentRevision BindingScope = "content_revision"
	BindingCatalogLogo     BindingScope = "catalog_logo"
)

type BindingScope string

func (s BindingScope) ServiceName() string {
	switch s {
	case BindingContentRevision:
		return "content"
	case BindingCatalogLogo:
		return "entities"
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

type PublishedAttachmentRef struct {
	MaterialID string
	RevisionID string
	FileID     string
}
