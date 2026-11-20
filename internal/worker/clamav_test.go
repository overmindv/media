package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestClamAVVerdicts(t *testing.T) {
	payload := strings.Repeat("harmless content\n", 5000)
	for _, tc := range []struct {
		name, response string
		clean, missing bool
	}{
		{name: "clean", response: "stream: OK\x00", clean: true},
		{name: "match", response: "stream: TestSignature FOUND\x00"},
		{name: "partial scan", response: "stream: Heuristics.Limits.Exceeded FOUND\x00"},
		{name: "error ending in OK", response: "stream: ERROR OK\x00", missing: true},
		{name: "truncated", response: "stream: OK", missing: true},
		{name: "oversized", response: strings.Repeat("X", 4096) + "\x00", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := serveScan(t, func(conn net.Conn) error {
				var got bytes.Buffer
				err := readScan(conn, &got)
				if err != nil {
					return err
				}
				if got.String() != payload {
					return errors.New("scan input changed")
				}
				_, err = io.WriteString(conn, tc.response)
				return err
			})
			clean, err := NewClamAV(addr, time.Second).Scan(context.Background(), strings.NewReader(payload))
			if clean != tc.clean || (err != nil) != tc.missing || (tc.missing && !errors.Is(err, errScanUnavailable)) {
				t.Errorf("clean = %t, err = %v", clean, err)
			}
		})
	}
}

func TestClamAVCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := serveScan(t, func(conn net.Conn) error {
		if err := readScan(conn, io.Discard); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if clean, err := NewClamAV(addr, time.Minute).Scan(ctx, strings.NewReader("file")); clean || !errors.Is(err, context.Canceled) {
		t.Errorf("clean = %t, err = %v", clean, err)
	}
}

func serveScan(t *testing.T, handle func(net.Conn) error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		done <- handle(conn)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return listener.Addr().String()
}

func readScan(conn io.Reader, dst io.Writer) error {
	reader := bufio.NewReader(conn)
	command, err := reader.ReadString(0)
	if err != nil || command != "zINSTREAM\x00" {
		return fmt.Errorf("unexpected ClamAV command %q: %v", command, err)
	}
	var length uint32
	for {
		if err := binary.Read(reader, binary.BigEndian, &length); err != nil || length == 0 {
			return err
		}
		if _, err := io.CopyN(dst, reader, int64(length)); err != nil {
			return err
		}
	}
}
