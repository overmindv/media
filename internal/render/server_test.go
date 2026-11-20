package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOversizedImageRejectedBeforeDecoder(t *testing.T) {
	var data bytes.Buffer
	data.WriteString("\x89PNG\r\n\x1a\n")
	header := [17]byte{'I', 'H', 'D', 'R'}
	binary.BigEndian.PutUint32(header[4:8], 7000)
	binary.BigEndian.PutUint32(header[8:12], 7000)
	header[12], header[13] = 8, 2
	_ = binary.Write(&data, binary.BigEndian, uint32(13))
	data.Write(header[:])
	_ = binary.Write(&data, binary.BigEndian, crc32.ChecksumIEEE(header[:]))
	data.WriteString("\x00\x00\x00\x00IDAT\x35\xaf\x06\x1e")
	called := false
	handler := newHandler(func(context.Context, string, string, int) error {
		called = true
		return nil
	}, time.Second)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/render?width=128", &data))
	if response.Code != http.StatusRequestEntityTooLarge || called {
		t.Error("large decoded image reached the decoder", response.Code, called)
	}
}

func TestTimeoutRemovesScratchDirectory(t *testing.T) {
	var source bytes.Buffer
	if err := png.Encode(&source, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	var scratch string
	handler := newHandler(func(ctx context.Context, input, output string, _ int) error {
		scratch = filepath.Dir(input)
		if err := os.WriteFile(output, []byte("fixture"), 0600); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}, 100*time.Millisecond)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/render?width=128", &source))
	if response.Code != http.StatusUnprocessableEntity || scratch == "" {
		t.Fatal("renderer did not reach and time out the job", response.Code)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Error("timed out job left scratch files", err)
	}
}
