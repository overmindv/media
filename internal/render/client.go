package render

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	dialer := net.Dialer{Timeout: 2 * time.Second}

	return &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			MaxConnsPerHost:        1,
			MaxIdleConnsPerHost:    1,
			IdleConnTimeout:        30 * time.Second,
			MaxResponseHeaderBytes: 4096,
			DisableCompression:     true,
		},
		Timeout:       jobTimeout + 5*time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Render(ctx context.Context, path string, widths []int, consume func(int, io.Reader, int64) error) error {
	input, err := os.Open(path) //nolint:gosec // snapshot created by Media worker
	if err != nil {
		return fmt.Errorf("open image snapshot: %w", err)
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return fmt.Errorf("stat image snapshot: %w", err)
	}
	if info.Size() <= 0 || info.Size() > MaxInputBytes {
		return ErrTooLarge
	}
	query := make(url.Values, 1)
	for _, width := range widths {
		query.Add("width", strconv.Itoa(width))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://renderer/render?"+query.Encode(), input)
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	response, err := c.http.Do(request)
	if err != nil {
		return errors.Join(ErrUnavailable, ctx.Err())
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusUnprocessableEntity:
		return ErrRejected
	default:
		return ErrUnavailable
	}
	for _, width := range widths {
		var size uint32
		if err := binary.Read(response.Body, binary.BigEndian, &size); err != nil || size == 0 || size > MaxOutputBytes {
			return ErrUnavailable
		}
		part := &io.LimitedReader{R: response.Body, N: int64(size)}
		if err := consume(width, part, int64(size)); err != nil {
			return err
		}
		if part.N != 0 {
			return ErrUnavailable
		}
	}
	var extra [1]byte
	if _, err := io.ReadFull(response.Body, extra[:]); err != io.EOF {
		return ErrUnavailable
	}

	return nil
}
