package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
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
	t.Parallel()

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
	const tagText = "tEXtComment\x00" + marker
	tag := []byte(tagText)
	var output bytes.Buffer
	payload := pngBytes.Bytes()
	output.Write(payload[:len(payload)-12])
	output.Write(binary.BigEndian.AppendUint32(nil, uint32(len(tagText)-4)))
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

type partialInvalidRenderer struct{}

func (partialInvalidRenderer) Render(_ context.Context, _ string, widths []int, consume func(int, io.Reader, int64) error) error {
	output := []string{
		"UklGRjIAAABXRUJQVlA4ICYAAACwAgCdASoMABQAPpE6l0eloyIhMAgAsBIJaQAAeyAA/vjPeMAAAA==",
		"Y29ycnVwdA==",
	}
	for i, value := range output {
		part, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return err
		}
		if err := consume(widths[i], bytes.NewReader(part), int64(len(part))); err != nil {
			return err
		}
	}

	return nil
}

func TestRendererBatchRejectsInvalidWebPBeforeUpload(t *testing.T) {
	t.Parallel()

	objects := &previewStore{}
	p := Processor{storage: objects, renderer: partialInvalidRenderer{}}
	if _, err := p.createVariants(t.Context(), "", "blob", domain.PublicVariantPrefix+"blob/", 200, nil); err == nil {
		t.Fatal("invalid WebP was accepted")
	}
	if objects.data != "" {
		t.Error("a preview was uploaded before the entire batch was validated")
	}
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
	file, err := os.Create(source) //nolint:gosec // path in the benchmark temp directory
	if err != nil {
		b.Fatal(err)
	}
	input := image.NewRGBA(image.Rect(0, 0, 1024, 768))
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // repeatable benchmark input
	for i := 0; i < len(input.Pix); i += 4 {
		binary.BigEndian.PutUint32(input.Pix[i:], rng.Uint32())
		input.Pix[i+3] = 255
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
				// #nosec G204 -- fixed command with paths in the benchmark temp directory.
				cmd := exec.CommandContext(b.Context(), "/usr/bin/vipsthumbnail", source,
					"--size", strconv.Itoa(min(width, 1024))+"x>", "--output", path+"[Q=88,strip]")
				cmd.Env = append(os.Environ(), "VIPS_CONCURRENCY=1")
				if err := cmd.Run(); err != nil {
					b.Fatal(err)
				}
				variant, preview, openErr := openVariant(path, "blob", name, name, width)
				if openErr != nil {
					b.Fatal(openErr)
				}
				putErr := processor.storeVariant(b.Context(), variant, preview)
				if closeErr := errors.Join(putErr, preview.Close()); closeErr != nil {
					b.Fatal(closeErr)
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
