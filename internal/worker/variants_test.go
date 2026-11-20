package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/service"
)

func TestSmallImageGetsStrippedPreview(t *testing.T) {
	if _, err := exec.LookPath("vipsthumbnail"); err != nil {
		t.Skip("vipsthumbnail is required")
	}
	source := filepath.Join(t.TempDir(), "fixture.png")
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 12, 20))); err != nil {
		t.Fatal(err)
	}
	const marker = "fixture-not-public"
	tag := []byte("tEXtComment\x00" + marker)
	var output bytes.Buffer
	payload := pngBytes.Bytes()
	output.Write(payload[:len(payload)-12])
	output.Write(binary.BigEndian.AppendUint32(nil, uint32(len(tag)-4)))
	output.Write(tag)
	output.Write(binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(tag)))
	output.Write(payload[len(payload)-12:])
	if err := os.WriteFile(source, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	objects := &previewStore{}
	p := Processor{storage: objects, config: config.Config{S3: config.S3{MediaBucket: "media"}}}
	prefix := domain.PublicVariantPrefix + "blob/"
	variants, err := p.createVariants(t.Context(), source, "blob", prefix, 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Fatal("small image must produce one preview", len(variants))
	}
	v := variants[0]
	if v.Width != 12 || v.Height != 20 || v.Name != "w128" || v.ObjectKey != prefix+"w128.webp" {
		t.Error("unexpected preview dimensions or key", v)
	}
	if strings.Contains(objects.data, marker) {
		t.Error("metadata survived the WebP conversion")
	}
}

type previewStore struct {
	service.ObjectStorage
	data string
}

func (s *previewStore) Put(_ context.Context, _, _, _ string, body io.Reader, _ int64) error {
	data, err := io.ReadAll(body)
	s.data = string(data)
	return err
}
