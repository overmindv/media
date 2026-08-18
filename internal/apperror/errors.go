package apperror

import "fmt"

const (
	ValidationError       = "VALIDATION_ERROR"
	UnsupportedMediaType  = "UNSUPPORTED_MEDIA_TYPE"
	FileTooLarge          = "FILE_TOO_LARGE"
	FileNotFound          = "FILE_NOT_FOUND"
	FileNotReady          = "FILE_NOT_READY"
	UploadExpired         = "UPLOAD_EXPIRED"
	UploadIntegrityFailed = "UPLOAD_INTEGRITY_FAILED"
	FileInfected          = "FILE_INFECTED"
	FileInUse             = "FILE_IN_USE"
	PermissionDenied      = "PERMISSION_DENIED"
	InternalError         = "INTERNAL_ERROR"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

// Error возвращает безопасное строковое представление публичной ошибки.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New создаёт публичную ошибку с машинным кодом и HTTP status.
func New(code, message string, status int) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Status:  status,
	}
}
