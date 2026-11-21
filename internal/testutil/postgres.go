//go:build component

package testutil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/parker"
)

func UploadQuota() domain.UploadQuota {
	return domain.UploadQuota{MaxBytes: 5 << 30, MaxMultipartSessions: 4}
}

func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return PostgresWithSeed(t, "", nil)
}

func PostgresWithSeed(t testing.TB, migration string, seed func(*pgxpool.Pool)) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("COMPONENT_TEST_DSN")
	if dsn == "" {
		t.Fatal("COMPONENT_TEST_DSN is required for component tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "media_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	migrationDSN := stdlib.RegisterConnConfig(config.ConnConfig)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(migrationDSN) })
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dir := filepath.Join("..", "..", "migrations")
	if seed != nil {
		before := t.TempDir()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".sql" || name >= migration {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(before, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := parker.RunMigrate(before, migrationDSN, "up"); err != nil {
			t.Fatal(err)
		}
		seed(pool)
	}
	if err := parker.RunMigrate(dir, migrationDSN, "up"); err != nil {
		t.Fatal(err)
	}
	return pool
}
