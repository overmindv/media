package domain

import "time"

type Purpose string

const (
	PurposeAvatar       Purpose = "avatar"
	PurposeCatalogLogo  Purpose = "catalog_logo"
	PurposeContentImage Purpose = "content_image"
	PurposeAttachment   Purpose = "attachment"
	PurposeArchive      Purpose = "archive"
)

type Visibility string

const (
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

type Status string

const (
	StatusPendingUpload Status = "pending_upload"
	StatusQuarantined   Status = "quarantined"
	StatusProcessing    Status = "processing"
	StatusReady         Status = "ready"
	StatusRejected      Status = "rejected"
	StatusDeleted       Status = "deleted"
)

type UploadMode string

const (
	UploadModeSingle    UploadMode = "single"
	UploadModeMultipart UploadMode = "multipart"
)

// File описывает логический медиафайл и не содержит бинарных данных.
type File struct {
	ID                  string     `json:"id"`
	OwnerUserID         string     `json:"owner_user_id"`
	Purpose             Purpose    `json:"purpose"`
	Visibility          Visibility `json:"visibility"`
	OriginalName        string     `json:"original_name"`
	DeclaredContentType string     `json:"declared_content_type"`
	DetectedContentType string     `json:"detected_content_type"`
	SizeBytes           int64      `json:"size_bytes"`
	ChecksumSHA256      string     `json:"checksum_sha256"`
	Status              Status     `json:"status"`
	FailureCode         string     `json:"failure_code"`
	PublicURL           string     `json:"public_url,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
}

// UploadSession хранит состояние прямой загрузки в object storage.
type UploadSession struct {
	FileID       string
	Mode         UploadMode
	Bucket       string
	ObjectKey    string
	MultipartID  string
	ExpectedSize int64
	Checksum     string
	ContentType  string
	ExpiresAt    time.Time
	CompletedAt  *time.Time
}

// Actor содержит доверенный пользовательский контекст от API Gateway.
type Actor struct {
	UserID string
	Roles  []string
}

// IsAdmin сообщает, имеет ли actor административную роль.
func (a Actor) IsAdmin() bool {
	for _, role := range a.Roles {
		if role == "admin" || role == "superuser" {
			return true
		}
	}

	return false
}

// CanRead проверяет право actor читать файл.
func (f File) CanRead(actor Actor) bool {
	if f.Visibility == VisibilityPublic && f.Status == StatusReady {
		return true
	}

	return actor.UserID != "" && (actor.UserID == f.OwnerUserID || actor.IsAdmin())
}

// CanDelete проверяет право actor удалить файл.
func (f File) CanDelete(actor Actor) bool {
	return actor.UserID != "" && (actor.UserID == f.OwnerUserID || actor.IsAdmin())
}

// CreateUploadInput содержит проверенные декларативные параметры файла.
type CreateUploadInput struct {
	OriginalName string     `json:"original_name"`
	ContentType  string     `json:"content_type"`
	SizeBytes    int64      `json:"size_bytes"`
	Checksum     string     `json:"checksum_sha256"`
	Purpose      Purpose    `json:"purpose"`
	Visibility   Visibility `json:"visibility"`
}

// UploadTarget описывает данные, необходимые браузеру для прямой загрузки.
type UploadTarget struct {
	FileID      string            `json:"file_id"`
	Mode        UploadMode        `json:"mode"`
	URL         string            `json:"url,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	MultipartID string            `json:"multipart_upload_id,omitempty"`
	PartSize    int64             `json:"part_size,omitempty"`
	ExpiresAt   time.Time         `json:"expires_at"`
}

type CompletedPart struct {
	PartNumber int32  `json:"part_number"`
	ETag       string `json:"etag"`
}

type UploadPart struct {
	PartNumber int32     `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type FileList struct {
	Items  []File `json:"items"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

type Download struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PublicFile содержит только immutable URL готового публичного файла.
type PublicFile struct {
	FileID string            `json:"file_id"`
	URLs   map[string]string `json:"urls"`
}

// AvatarBinding описывает единственную активную связь аватара с пользователем.
type AvatarBinding struct {
	UserID string  `json:"user_id"`
	FileID *string `json:"file_id"`
}
