package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/render"
	"github.com/overmindv/media/internal/service"
)

func TestSmallImageGetsStrippedPreview(t *testing.T) {
	socket := os.Getenv("MEDIA_TEST_RENDER_SOCKET")
	if socket == "" {
		t.Skip("isolated renderer test socket is required")
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
	p := Processor{storage: objects, renderer: render.NewClient(socket), config: config.Config{S3: config.S3{MediaBucket: "media"}}}
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

func BenchmarkPreviewIsolation(b *testing.B) {
	socket := os.Getenv("MEDIA_TEST_RENDER_SOCKET")
	if socket == "" {
		b.Skip("isolated renderer test socket is required")
	}
	dir := b.TempDir()
	source := filepath.Join(dir, "benchmark.jpg")
	file, err := os.Create(source)
	if err != nil {
		b.Fatal(err)
	}
	input := image.NewRGBA(image.Rect(0, 0, 1024, 768))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < len(input.Pix); i += 4 {
		input.Pix[i], input.Pix[i+1], input.Pix[i+2], input.Pix[i+3] = byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32()), 255
	}
	if err := jpeg.Encode(file, input, &jpeg.Options{Quality: 95}); err != nil {
		_ = file.Close()
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	processor := Processor{storage: discardPreviews{}, renderer: render.NewClient(socket)}
	b.Run("direct", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, width := range []int{128, 320, 768, 1440} {
				name := "w" + strconv.Itoa(width)
				path := filepath.Join(dir, name+".webp")
				cmd := exec.CommandContext(b.Context(), "/usr/bin/vipsthumbnail", source,
					"--size", strconv.Itoa(min(width, 1024))+"x>", "--output", path+"[Q=88,strip]")
				cmd.Env = append(os.Environ(), "VIPS_CONCURRENCY=1")
				if err := cmd.Run(); err != nil {
					b.Fatal(err)
				}
				if _, err := processor.uploadVariant(b.Context(), path, "blob", name, name); err != nil {
					b.Fatal(err)
				}
				_ = os.Remove(path)
			}
		}
	})
	b.Run("isolated", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := processor.createVariants(b.Context(), source, "blob", "", 1024, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type discardPreviews struct{ service.ObjectStorage }

func (discardPreviews) Put(_ context.Context, _, _, _ string, body io.Reader, _ int64) error {
	_, err := io.Copy(io.Discard, body)
	return err
}
