//go:build !linux

package render

import (
	"context"
	"os"
)

func runThumbnail(context.Context, string, string, int) error {
	return ErrUnavailable
}

func openOutput(string) (*os.File, error) {
	return nil, ErrUnavailable
}
