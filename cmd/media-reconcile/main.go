package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/config"
	"github.com/overmindv/media/internal/inventory"
	"github.com/overmindv/media/internal/repository"
	"github.com/overmindv/media/internal/storage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("media-reconcile", flag.ContinueOnError)
	flags.SetOutput(errOut)
	scope := flags.String("scope", "originals", "originals, variants or uploads")
	bucket := flags.String("bucket", "", "storage bucket override")
	metadataBucket := flags.String("metadata-bucket", "", "original bucket name stored for upload sessions")
	after := flags.String("after", "", "resume after this key")
	pageSize := flags.Int("page-size", 500, "page size (1-1000)")
	maxEntries := flags.Int("max-entries", 50_000, "maximum merged keys per run")
	grace := flags.Duration("grace", time.Hour, "minimum age of a reported finding")
	timeout := flags.Duration("timeout", 2*time.Minute, "read-only snapshot and scan timeout")
	baseline := flags.String("as-of", "", "RFC3339 reference time for the grace window")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *pageSize < 1 || *pageSize > 1000 || *maxEntries < 1 || *maxEntries > 1_000_000 ||
		*grace < time.Second || *timeout < time.Second || *timeout > 10*time.Minute {
		return fmt.Errorf("invalid inventory limits")
	}
	var defaultBucket string
	switch *scope {
	case "originals", "variants":
		defaultBucket = "media"
	case "uploads":
		defaultBucket = "media-quarantine"
	default:
		return fmt.Errorf("invalid inventory scope")
	}
	if *bucket == "" {
		*bucket = defaultBucket
	}
	if *metadataBucket == "" {
		*metadataBucket = defaultBucket
	}
	prefix := *scope + "/"
	if *after != "" && !strings.HasPrefix(*after, prefix) {
		return fmt.Errorf("resume key is outside the selected scope")
	}
	now := time.Now().UTC()
	asOf := now
	if *baseline != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *baseline)
		if err != nil || parsed.After(now) {
			return fmt.Errorf("invalid inventory reference time")
		}
		asOf = parsed.UTC()
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	dsn := os.Getenv("MEDIA_RECONCILE_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("MEDIA_RECONCILE_DATABASE_URL is required")
	}
	s3Config, err := storageConfig()
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect inventory database")
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("start read-only inventory snapshot: %w", err)
	}
	defer tx.Rollback(ctx)
	metadata, err := repository.InventoryPages(tx, *metadataBucket, prefix)
	if err != nil {
		return err
	}
	s3, err := storage.NewS3(ctx, s3Config)
	if err != nil {
		return fmt.Errorf("create inventory object client")
	}
	encoder := json.NewEncoder(out)
	summary, err := inventory.Compare(ctx, metadata, s3.InventoryPages(*bucket, prefix), inventory.Options{
		Prefix: prefix, After: *after, PageSize: *pageSize, MaxEntries: *maxEntries, Cutoff: asOf.Add(-*grace),
	}, func(item inventory.Finding) error {
		return encoder.Encode(item)
	})
	if err != nil {
		return err
	}
	return encoder.Encode(struct {
		inventory.Summary
		Scope string    `json:"scope"`
		AsOf  time.Time `json:"as_of"`
	}{Summary: summary, Scope: *scope, AsOf: asOf})
}

func storageConfig() (config.S3, error) {
	endpoint := os.Getenv("MEDIA_RECONCILE_S3_ENDPOINT")
	address, err := url.Parse(endpoint)
	if err != nil || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" ||
		(address.Scheme != "http" && address.Scheme != "https") {
		return config.S3{}, fmt.Errorf("MEDIA_RECONCILE_S3_ENDPOINT must be an HTTP(S) endpoint without credentials")
	}
	access := os.Getenv("MEDIA_RECONCILE_S3_ACCESS_KEY")
	secret := os.Getenv("MEDIA_RECONCILE_S3_SECRET_KEY")
	if access == "" || secret == "" {
		return config.S3{}, fmt.Errorf("dedicated inventory S3 credentials are required")
	}
	region := os.Getenv("MEDIA_RECONCILE_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return config.S3{Region: region, Endpoint: endpoint, PublicEndpoint: endpoint, PathStyle: true, AccessKey: access, SecretKey: secret}, nil
}
