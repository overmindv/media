package inventory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Entry struct {
	Key           string
	ExpectedSince time.Time
	CleanedAt     time.Time
	LastModified  time.Time
}

type Page func(ctx context.Context, after string, size int) ([]Entry, bool, error)

type Finding struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

type Summary struct {
	Kind       string `json:"kind"`
	Scanned    int    `json:"scanned"`
	Missing    int    `json:"missing"`
	Orphans    int    `json:"orphans"`
	Reappeared int    `json:"reappeared"`
	Deferred   int    `json:"deferred"`
	Complete   bool   `json:"complete"`
	NextAfter  string `json:"next_after,omitempty"`
}

type Options struct {
	Prefix     string
	After      string
	PageSize   int
	MaxEntries int
	Cutoff     time.Time
}

func Compare(ctx context.Context, metadata, objects Page, opts Options, emit func(Finding) error) (Summary, error) {
	summary := Summary{Kind: "summary"}
	if opts.Prefix == "" || opts.PageSize < 1 || opts.PageSize > 1000 || opts.MaxEntries < 1 ||
		(opts.After != "" && !strings.HasPrefix(opts.After, opts.Prefix)) || opts.Cutoff.IsZero() {
		return summary, fmt.Errorf("invalid inventory options")
	}
	start := opts.After
	if start == "" {
		start = opts.Prefix
	}
	db := cursor{load: metadata, prefix: opts.Prefix, after: start, size: opts.PageSize}
	s3 := cursor{load: objects, prefix: opts.Prefix, after: start, size: opts.PageSize}
	last := start
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		want, err := db.peek(ctx)
		if err != nil {
			return summary, fmt.Errorf("metadata page: %w", err)
		}
		have, err := s3.peek(ctx)
		if err != nil {
			return summary, fmt.Errorf("object page: %w", err)
		}
		if want.Key == "" && have.Key == "" {
			summary.Complete = true
			return summary, nil
		}
		if summary.Scanned == opts.MaxEntries {
			summary.NextAfter = last
			return summary, nil
		}
		var kind string
		var since time.Time
		switch {
		case want.Key == "" || (have.Key != "" && have.Key < want.Key):
			last, kind, since = have.Key, "orphan", have.LastModified
			s3.advance()
		case have.Key == "" || want.Key < have.Key:
			last, since = want.Key, want.ExpectedSince
			if !since.IsZero() {
				kind = "missing"
			}
			db.advance()
		default:
			last = want.Key
			if !want.CleanedAt.IsZero() {
				kind, since = "reappeared", want.CleanedAt
				if have.LastModified.After(since) {
					since = have.LastModified
				}
			}
			db.advance()
			s3.advance()
		}
		summary.Scanned++
		if kind == "" {
			continue
		}
		if since.IsZero() || since.After(opts.Cutoff) {
			summary.Deferred++
			continue
		}
		if err := emit(Finding{Kind: kind, Key: last}); err != nil {
			return summary, fmt.Errorf("write finding: %w", err)
		}
		switch kind {
		case "missing":
			summary.Missing++
		case "orphan":
			summary.Orphans++
		case "reappeared":
			summary.Reappeared++
		}
	}
}

type cursor struct {
	load   Page
	prefix string
	after  string
	size   int
	page   []Entry
	done   bool
}

func (c *cursor) peek(ctx context.Context) (Entry, error) {
	if len(c.page) == 0 && !c.done {
		page, more, err := c.load(ctx, c.after, c.size)
		if err != nil {
			return Entry{}, err
		}
		if len(page) > c.size || (more && len(page) == 0) {
			return Entry{}, fmt.Errorf("inventory page cannot advance")
		}
		for _, entry := range page {
			if entry.Key <= c.after || !strings.HasPrefix(entry.Key, c.prefix) {
				return Entry{}, fmt.Errorf("inventory keys out of order or scope")
			}
			c.after = entry.Key
		}
		c.page, c.done = page, !more
	}
	if len(c.page) == 0 {
		return Entry{}, nil
	}
	return c.page[0], nil
}

func (c *cursor) advance() { c.page = c.page[1:] }
