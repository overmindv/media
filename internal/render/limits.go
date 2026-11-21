package render

import (
	"errors"
	"image"
	"time"
)

const (
	SocketPath     = "/run/media-renderer/render.sock"
	MaxInputBytes  = 20 << 20
	MaxOutputBytes = 8 << 20
	MaxDimension   = 8192
	MaxPixels      = (160 << 20) / 8
	jobTimeout     = 25 * time.Second
)

var (
	ErrUnavailable = errors.New("image renderer unavailable")
	ErrRejected    = errors.New("image renderer rejected input")
	ErrTooLarge    = errors.New("image exceeds render limits")
)

func CheckDimensions(cfg image.Config, maxPixels int64) error {
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxDimension || cfg.Height > MaxDimension ||
		int64(cfg.Width)*int64(cfg.Height) > min(maxPixels, MaxPixels) {
		return ErrTooLarge
	}

	return nil
}

func validWidth(width int) bool {
	return width == 128 || width == 320 || width == 768 || width == 1440
}
