//go:build component

package repository

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/overmindv/media/internal/domain"
	"github.com/overmindv/media/internal/testutil"
)

func TestResolvePublicPreviews(t *testing.T) {
	repo := New(testutil.Postgres(t), false)
	ids := seedPreviews(t, repo, 4)
	ctx := t.Context()
	if _, err := repo.pool.Exec(ctx, "UPDATE files SET visibility='private' WHERE id=$1", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, "UPDATE blobs SET scan_verified=false WHERE id=(SELECT blob_id FROM files WHERE id=$1)", ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, "UPDATE blob_variants SET object_key=replace(object_key,$2,'variants/legacy/') WHERE blob_id=(SELECT blob_id FROM files WHERE id=$1)", ids[3], domain.PublicVariantPrefix); err != nil {
		t.Fatal(err)
	}
	files, err := repo.ResolvePublicFiles(ctx, ids, []string{"w128", "original"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FileID != ids[0] || len(files[0].URLs) != 1 || files[0].URLs["w128"] == "" {
		t.Error("only a verified public preview should be returned", files)
	}
	avatar, err := repo.GetFile(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAvatarBinding(ctx, avatar.OwnerUserID, &avatar.ID); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkResolvePublicPreviews(b *testing.B) {
	repo := New(testutil.Postgres(b), false)
	ids := seedPreviews(b, repo, 2000)[:100]
	variants := []string{"w128", "w320", "w768"}
	b.Run("previous_query", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rows, err := repo.pool.Query(b.Context(), `
                SELECT f.id, 'original', b.object_key FROM files f JOIN blobs b ON b.id=f.blob_id
                 WHERE f.id=ANY($1::uuid[]) AND f.status='ready' AND f.visibility='public'
                   AND f.deleted_at IS NULL AND b.deleted_at IS NULL AND b.scan_status='clean' AND (b.scan_verified OR $3)
                UNION ALL
                SELECT f.id, v.name, v.object_key FROM files f JOIN blobs b ON b.id=f.blob_id
                 JOIN blob_variants v ON v.blob_id=b.id
                 WHERE f.id=ANY($1::uuid[]) AND f.status='ready' AND f.visibility='public'
                   AND f.deleted_at IS NULL AND b.deleted_at IS NULL AND b.scan_status='clean' AND (b.scan_verified OR $3)
                   AND v.name=ANY($2::text[])`, ids, variants, false)
			if err != nil {
				b.Fatal(err)
			}
			_, err = collectPublicPreviews(rows, ids)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("preview_only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := repo.ResolvePublicFiles(b.Context(), ids, variants); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func seedPreviews(t testing.TB, repo *Postgres, count int) []string {
	t.Helper()
	rows, err := repo.pool.Query(t.Context(), `
        WITH fixtures AS MATERIALIZED (
            SELECT gen_random_uuid() id, gen_random_uuid() owner, gen_random_uuid() blob
             FROM generate_series(1,$1)
        ), seed_blobs AS (
            INSERT INTO blobs (id,checksum_sha256,size_bytes,visibility,detected_content_type,object_key,scan_status,scan_verified)
            SELECT blob,id::text,42,'public','image/png','originals/'||id,'clean',true FROM fixtures
            RETURNING id
        ), seed_variants AS (
            INSERT INTO blob_variants (id,blob_id,name,format,width,height,size_bytes,checksum_sha256,object_key)
            SELECT gen_random_uuid(),id,'w128','webp',12,12,16,id::text,$2||id||'/w128.webp' FROM seed_blobs
        )
        INSERT INTO files (id,blob_id,owner_user_id,purpose,visibility,original_name,declared_content_type,size_bytes,checksum_sha256,status)
        SELECT f.id,b.id,f.owner,'avatar','public','fixture.png','image/png',42,f.id::text,'ready'
          FROM fixtures f JOIN seed_blobs b ON f.blob=b.id RETURNING id::text`, count, domain.PublicVariantPrefix)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
