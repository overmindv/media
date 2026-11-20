package inventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestComparePagesAndResume(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	metadata := []Entry{
		{Key: "originals/a", ExpectedSince: old},
		{Key: "originals/d", CleanedAt: old},
		{Key: "originals/e"},
		{Key: "originals/g", ExpectedSince: now},
		{Key: "originals/h", ExpectedSince: old},
	}
	objects := []Entry{
		{Key: "originals/b", LastModified: old},
		{Key: "originals/c", LastModified: now},
		{Key: "originals/d", LastModified: old},
		{Key: "originals/e", LastModified: old},
		{Key: "originals/h", LastModified: old},
	}
	var findings []Finding
	emit := func(f Finding) error {
		findings = append(findings, f)
		return nil
	}
	opts := Options{Prefix: "originals/", PageSize: 2, MaxEntries: 3, Cutoff: now.Add(-time.Hour)}
	first, err := Compare(t.Context(), pages(metadata), pages(objects), opts, emit)
	if err != nil || first.Complete || first.NextAfter != "originals/c" || first.Scanned != 3 || first.Deferred != 1 {
		t.Fatal("unexpected first page", first, err)
	}
	opts.After, opts.MaxEntries = first.NextAfter, 4
	last, err := Compare(t.Context(), pages(metadata), pages(objects), opts, emit)
	if err != nil || !last.Complete || last.NextAfter != "" || last.Scanned != 4 || last.Deferred != 1 {
		t.Fatal("unexpected final page", last, err)
	}
	want := []Finding{{"missing", "originals/a"}, {"orphan", "originals/b"}, {"reappeared", "originals/d"}}
	if !slices.Equal(findings, want) {
		t.Error("wrong findings", findings)
	}
}

func TestCompareRejectsInvalidSourceAndFailedWrites(t *testing.T) {
	opts := Options{Prefix: "uploads/", PageSize: 1, MaxEntries: 10, Cutoff: time.Now()}
	unordered := func(context.Context, string, int) ([]Entry, bool, error) {
		return []Entry{{Key: "outside/fixture"}}, false, nil
	}
	if _, err := Compare(t.Context(), pages(nil), unordered, opts, func(Finding) error { return nil }); err == nil {
		t.Error("accepted out-of-scope listing")
	}
	objects := pages([]Entry{{Key: "uploads/a", LastModified: opts.Cutoff.Add(-time.Hour)}})
	if _, err := Compare(t.Context(), pages(nil), objects, opts, func(Finding) error { return io.ErrClosedPipe }); !errors.Is(err, io.ErrClosedPipe) {
		t.Error("failed report write was not propagated", err)
	}
}

func pages(entries []Entry) Page {
	return func(_ context.Context, after string, size int) ([]Entry, bool, error) {
		start, found := slices.BinarySearchFunc(entries, after, func(entry Entry, key string) int {
			return strings.Compare(entry.Key, key)
		})
		if found {
			start++
		}
		end := min(start+size, len(entries))
		return entries[start:end], end < len(entries), nil
	}
}

func BenchmarkInventoryMerge(b *testing.B) {
	entries := make([]Entry, 50_000)
	for i := range entries {
		entries[i].Key = fmt.Sprintf("originals/%06d", i)
	}
	opts := Options{Prefix: "originals/", PageSize: 500, MaxEntries: len(entries), Cutoff: time.Now()}
	b.ReportAllocs()
	for b.Loop() {
		if result, err := Compare(b.Context(), pages(entries), pages(entries), opts, func(Finding) error { return nil }); err != nil || !result.Complete {
			b.Fatal(result, err)
		}
	}
}
