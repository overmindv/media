package worker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

var errScanUnavailable = errors.New("ClamAV scan unavailable")

type ClamAV struct {
	address string
	timeout time.Duration
}

func NewClamAV(address string, timeout time.Duration) *ClamAV {
	return &ClamAV{address: address, timeout: timeout}
}

func (c *ClamAV) Scan(ctx context.Context, source io.Reader) (bool, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return false, errors.Join(errScanUnavailable, ctx.Err())
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return false, errScanUnavailable
	}
	if _, err := io.WriteString(conn, "zINSTREAM\x00"); err != nil {
		return false, errors.Join(errScanUnavailable, ctx.Err())
	}

	frame := make([]byte, 4+(32<<10))
	for {
		n, readErr := source.Read(frame[4:])
		if n > 0 {
			binary.BigEndian.PutUint32(frame[:4], uint32(n))
			if _, err := conn.Write(frame[:4+n]); err != nil {
				return false, errors.Join(errScanUnavailable, ctx.Err())
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, fmt.Errorf("read scan input: %w", readErr)
		}
	}
	clear(frame[:4])
	if _, err := conn.Write(frame[:4]); err != nil {
		return false, errors.Join(errScanUnavailable, ctx.Err())
	}
	response, err := bufio.NewReaderSize(conn, 4096).ReadSlice(0)
	if err != nil {
		return false, errors.Join(errScanUnavailable, ctx.Err())
	}
	reply, ok := strings.CutPrefix(string(response[:len(response)-1]), "stream: ")
	if !ok || strings.ContainsAny(reply, "\r\n") {
		return false, errScanUnavailable
	}
	if reply == "OK" {
		return true, nil
	}
	if signature, found := strings.CutSuffix(reply, " FOUND"); found && strings.TrimSpace(signature) != "" {
		return false, nil
	}

	return false, errScanUnavailable
}
