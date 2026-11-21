//go:build component

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/overmindv/media/internal/apperror"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/render"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/service"
	"github.com/overmindv/media/internal/testutil"
)

func BenchmarkScanSnapshot(b *testing.B) {
	addr := os.Getenv("MEDIA_TEST_CLAMAV_ADDR")
	if addr == "" {
		b.Skip("MEDIA_TEST_CLAMAV_ADDR is required")
	}
	scanner := NewClamAV(addr, 30*time.Second)
	data := bytes.Repeat([]byte("scan throughput fixture\n"), (8<<20)/24)
	for _, staged := range []bool{true, false} {
		name := "stream"
		if staged {
			name = "read_snapshot_again"
		}
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "snapshot")
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				snapshot, err := os.Create(path)
				if err != nil {
					b.Fatal(err)
				}
				var input io.Reader = io.TeeReader(bytes.NewReader(data), io.MultiWriter(snapshot, sha256.New()))
				if staged {
					if _, err := io.Copy(io.Discard, input); err != nil {
						_ = snapshot.Close()
						b.Fatal(err)
					}
					if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
						_ = snapshot.Close()
						b.Fatal(err)
					}
					input = snapshot
				}
				clean, scanErr := scanner.Scan(b.Context(), input)
				if err := errors.Join(scanErr, snapshot.Close()); err != nil || !clean {
					b.Fatal("scan verdict:", clean, err)
				}
			}
		})
	}
}

const cleanPDF = "%PDF-1.4\nclean scan fixture\n"
const markedPDF = "%PDF-1.4\nOvermind-fixture-marker-v1\n"

func TestPublicationRequiresScan(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Postgres(t)
	repo := repository.New(pool, false)
	localRepo := repository.New(pool, true)
	cfg := config.Config{S3: config.S3{MediaBucket: "media"}, PublicBaseURL: "https://media.invalid"}
	objects := scanStore{data: make(map[string]string)}
	localCfg := cfg
	localCfg.AllowUnscannedLocal = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	legacy := queuePDF(t, localRepo, objects.data, cleanPDF)
	local := NewProcessor(localRepo, objects, nil, localCfg, logger)
	if err := local.runOne(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := localRepo.GetObjectKey(ctx, legacy.ID, "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetObjectKey(ctx, legacy.ID, "original"); err == nil {
		t.Error("strict mode exposed a local blob")
	}
	localFiles := service.New(localRepo, objects, localCfg)
	localURL, err := localFiles.DownloadURL(ctx, legacy.ID, "", domain.Actor{UserID: legacy.OwnerUserID})
	if err != nil || !strings.HasPrefix(localURL.URL, "https://signed.invalid/originals/local/") {
		t.Errorf("want a signed local URL, got %v, err = %v", localURL, err)
	}

	clean := queuePDF(t, repo, objects.data, cleanPDF)
	if _, err := pool.Exec(ctx, "UPDATE media_jobs SET attempts=10 WHERE file_id=$1", clean.ID); err != nil {
		t.Fatal(err)
	}
	// A TCP endpoint that accepts the file but cannot return a verdict.
	addr := serveScan(t, func(conn net.Conn) error { return readScan(conn, io.Discard) })
	processor := NewProcessor(repo, objects, NewClamAV(addr, 2*time.Second), cfg, logger)
	if err := processor.runOne(ctx); !errors.Is(err, errScanUnavailable) {
		t.Fatal("want scanner failure:", err)
	}
	checkScanState(t, repo, clean.ID, domain.StatusQuarantined, "")
	if count, err := repo.ScanBacklog(ctx); err != nil || count != 1 {
		t.Errorf("scan backlog = %d, err = %v; want 1", count, err)
	}
	var delayed bool
	if err := pool.QueryRow(ctx, "SELECT available_at>NOW() AND status='pending' FROM media_jobs WHERE file_id=$1", clean.ID).Scan(&delayed); err != nil || !delayed {
		t.Error("scan outage did not schedule a delayed retry:", err)
	}
	files := service.New(repo, objects, cfg)
	if urls, err := files.ResolvePublicFiles(ctx, []string{legacy.ID, clean.ID}, []string{"w128"}); err != nil || len(urls) != 0 {
		t.Errorf("unverified URLs available: %v, err = %v", urls, err)
	}

	if _, err := pool.Exec(ctx, "UPDATE media_jobs SET available_at=NOW() WHERE file_id=$1", clean.ID); err != nil {
		t.Fatal(err)
	}
	objects.data["q/"+clean.ID] = cleanPDF
	scannerAddr := os.Getenv("MEDIA_TEST_CLAMAV_ADDR")
	if scannerAddr == "" {
		scannerAddr = scanReply(t, cleanPDF, "stream: OK\x00")
	}
	processor.scanner = NewClamAV(scannerAddr, 20*time.Second)
	if err := processor.runOne(ctx); err != nil {
		t.Fatal(err)
	}
	checkScanState(t, repo, clean.ID, domain.StatusReady, "")
	key, err := repo.GetObjectKey(ctx, clean.ID, "original")
	if err != nil || !strings.HasPrefix(key, "originals/verified/") || objects.data["media/"+key] != cleanPDF {
		t.Errorf("stored snapshot = %q, err = %v", objects.data["media/"+key], err)
	}
	if urls, err := files.ResolvePublicFiles(ctx, []string{legacy.ID, clean.ID}, []string{"w128"}); err != nil || len(urls) != 0 {
		t.Errorf("documents must not have public URLs, got %v, err = %v", urls, err)
	}

	infected := queuePDF(t, repo, objects.data, markedPDF)
	if os.Getenv("MEDIA_TEST_CLAMAV_ADDR") == "" {
		processor.scanner = NewClamAV(scanReply(t, markedPDF, "stream: TestSignature FOUND\x00"), time.Second)
	}
	if err := processor.runOne(ctx); err != nil {
		t.Fatal(err)
	}
	checkScanState(t, repo, infected.ID, domain.StatusRejected, apperror.FileInfected)
	if _, err := repo.GetObjectKey(ctx, infected.ID, "original"); err == nil {
		t.Error("rejected object has a URL")
	}
	if count, err := repo.ScanBacklog(ctx); err != nil || count != 0 {
		t.Errorf("scan backlog = %d, err = %v; want 0", count, err)
	}
}

func scanReply(t *testing.T, payload, response string) string {
	t.Helper()
	return serveScan(t, func(conn net.Conn) error {
		var data strings.Builder
		if err := readScan(conn, &data); err != nil {
			return err
		}
		if data.String() != payload {
			return errors.New("wrong scan payload")
		}
		_, err := io.WriteString(conn, response)
		return err
	})
}

func TestRendererUnavailableNeverPublishes(t *testing.T) {
	pool := testutil.Postgres(t)
	repo := repository.New(pool, false)
	var source bytes.Buffer
	if err := png.Encode(&source, image.NewRGBA(image.Rect(0, 0, 12, 12))); err != nil {
		t.Fatal(err)
	}
	objects := scanStore{data: make(map[string]string)}
	file := queueFixture(t, repo, objects.data, source.String(), "image/png", domain.PurposeContentImage, domain.VisibilityPublic)
	scanner := NewClamAV(scanReply(t, source.String(), "stream: OK\x00"), time.Second)
	cfg := config.Config{S3: config.S3{MediaBucket: "media"}, Limits: config.Limits{MaxImagePixels: render.MaxPixels},
		RenderSocket: filepath.Join(t.TempDir(), "offline.sock")}
	processor := NewProcessor(repo, objects, scanner, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := processor.runOne(t.Context()); !errors.Is(err, render.ErrUnavailable) {
		t.Fatal("want renderer outage:", err)
	}
	var state string
	if err := pool.QueryRow(t.Context(), "SELECT status FROM media_jobs WHERE file_id=$1", file.ID).Scan(&state); err != nil || state != "pending" {
		t.Error("renderer outage did not schedule retry", state, err)
	}
	checkScanState(t, repo, file.ID, domain.StatusProcessing, "")
	previews, err := repo.ResolvePublicFiles(t.Context(), []string{file.ID}, []string{"w128"})
	if err != nil || len(previews) != 0 {
		t.Error("unexpected public preview during renderer outage", previews, err)
	}
}

func queuePDF(t *testing.T, repo *repository.Postgres, objects map[string]string, data string) domain.File {
	t.Helper()
	return queueFixture(t, repo, objects, data, "application/pdf", domain.PurposeAttachment, domain.VisibilityPrivate)
}

func queueFixture(t *testing.T, repo *repository.Postgres, objects map[string]string, data, contentType string, purpose domain.Purpose, visibility domain.Visibility) domain.File {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	file := domain.File{
		ID: uuid.NewString(), OwnerUserID: uuid.NewString(), Purpose: purpose,
		Visibility: visibility, OriginalName: "fixture", DeclaredContentType: contentType,
		SizeBytes: int64(len(data)), ChecksumSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(data))),
		Status: domain.StatusPendingUpload, CreatedAt: now, UpdatedAt: now,
	}
	session := domain.UploadSession{
		FileID: file.ID, Mode: domain.UploadModeSingle, Bucket: "q", ObjectKey: file.ID,
		ExpectedSize: file.SizeBytes, Checksum: file.ChecksumSHA256, ContentType: file.DeclaredContentType,
		ExpiresAt: now.Add(time.Hour),
	}
	objects[session.Bucket+"/"+session.ObjectKey] = data
	if err := repo.CreateUpload(ctx, file, session); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteUpload(ctx, file.ID, func(context.Context, domain.UploadSession) (string, error) {
		return file.DeclaredContentType, nil
	}); err != nil {
		t.Fatal(err)
	}
	return file
}

func checkScanState(t *testing.T, repo *repository.Postgres, id string, status domain.Status, code string) {
	t.Helper()
	file, err := repo.GetFile(context.Background(), id)
	if err != nil || file.Status != status || file.FailureCode != code {
		t.Errorf("status = %q, failure = %q, err = %v; want %q/%q", file.Status, file.FailureCode, err, status, code)
	}
}

type scanStore struct {
	service.ObjectStorage
	data map[string]string
}

func (s scanStore) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	data, ok := s.data[bucket+"/"+key]
	if !ok {
		return nil, errors.New("fixture not found")
	}
	s.data[bucket+"/"+key] = "other harmless bytes"
	return io.NopCloser(strings.NewReader(data)), nil
}

func (s scanStore) Put(_ context.Context, bucket, key, _ string, data io.Reader, size int64) error {
	body, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	if int64(len(body)) != size {
		return errors.New("unexpected snapshot length")
	}
	s.data[bucket+"/"+key] = string(body)
	return nil
}

func (s scanStore) Delete(_ context.Context, bucket, key string) error {
	delete(s.data, bucket+"/"+key)
	return nil
}

func (s scanStore) PresignGet(_ context.Context, bucket, key string, _ time.Duration, _ bool) (string, error) {
	if _, ok := s.data[bucket+"/"+key]; !ok {
		return "", errors.New("fixture not found")
	}
	return "https://signed.invalid/" + key, nil
}
