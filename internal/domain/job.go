package domain

import "time"

type Job struct {
	ID       string
	FileID   string
	Kind     string
	Attempts int
}

type Blob struct {
	ID                  string
	ChecksumSHA256      string
	SizeBytes           int64
	Visibility          Visibility
	DetectedContentType string
	ObjectKey           string
}

type Variant struct {
	ID             string
	BlobID         string
	Name           string
	Format         string
	Width          int
	Height         int
	SizeBytes      int64
	ChecksumSHA256 string
	ObjectKey      string
	CreatedAt      time.Time
}
