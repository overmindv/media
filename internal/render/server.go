package render

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "golang.org/x/image/webp"
)

type jobRunner func(context.Context, string, string, int) error

func Serve(ctx context.Context) error {
	if info, err := os.Lstat(SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("renderer socket path is occupied")
		}
		conn, err := net.DialTimeout("unix", SocketPath, time.Second)
		if err == nil {
			_ = conn.Close()
			return errors.New("image renderer is already running")
		}
		if err := os.Remove(SocketPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(SocketPath, 0660); err != nil {
		return err
	}
	server := &http.Server{
		Handler:           newHandler(runThumbnail, jobTimeout),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      jobTimeout + 5*time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    4096,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newHandler(run jobRunner, timeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	slot := make(chan struct{}, 1)
	mux.HandleFunc("POST /render", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength <= 0 || r.ContentLength > MaxInputBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		names := r.URL.Query()["width"]
		if len(names) < 1 || len(names) > 4 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		widths := make([]int, 0, len(names))
		last := 0
		for _, name := range names {
			width, err := strconv.Atoi(name)
			if err != nil || width <= last || !validWidth(width) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			widths = append(widths, width)
			last = width
		}
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := renderBatch(ctx, w, r.Body, r.ContentLength, widths, run); err != nil {
			switch {
			case errors.Is(err, ErrTooLarge):
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			case errors.Is(err, ErrRejected), errors.Is(err, context.DeadlineExceeded):
				w.WriteHeader(http.StatusUnprocessableEntity)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
	})
	return mux
}

func renderBatch(ctx context.Context, w http.ResponseWriter, body io.Reader, size int64, widths []int, run jobRunner) error {
	dir, err := os.MkdirTemp("", "render-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	source, err := saveSource(dir, body, size)
	if err != nil {
		return err
	}
	outputs := make([]*os.File, 0, len(widths))
	sizes := make([]uint32, 0, len(widths))
	for _, width := range widths {
		path := filepath.Join(dir, strconv.Itoa(width)+".webp")
		if err := run(ctx, source, path, width); err != nil {
			return err
		}
		file, err := openOutput(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxOutputBytes {
			return ErrRejected
		}
		outputs = append(outputs, file)
		sizes = append(sizes, uint32(info.Size()))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	for index, file := range outputs {
		if err := binary.Write(w, binary.BigEndian, sizes[index]); err != nil {
			break
		}
		if _, err := io.CopyN(w, file, int64(sizes[index])); err != nil {
			break
		}
	}
	return nil
}

func saveSource(dir string, body io.Reader, size int64) (string, error) {
	path := filepath.Join(dir, "source")
	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := io.CopyN(file, body, size); err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	cfg, format, err := image.DecodeConfig(file)
	if err != nil {
		return "", ErrRejected
	}
	if err := CheckDimensions(cfg, MaxPixels); err != nil {
		return "", err
	}
	var ext string
	switch format {
	case "jpeg":
		ext = ".jpg"
	case "png":
		ext = ".png"
	case "webp":
		ext = ".webp"
	default:
		return "", ErrRejected
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(path, path+ext); err != nil {
		return "", err
	}
	return path + ext, nil
}
