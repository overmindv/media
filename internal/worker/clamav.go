package worker

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type ClamAV struct {
	address string
	timeout time.Duration
}

// NewClamAV создаёт потоковый clamd client.
func NewClamAV(address string, timeout time.Duration) *ClamAV {
	return &ClamAV{address: address, timeout: timeout}
}

// Ping проверяет готовность clamd через PING command.
func (c *ClamAV) Ping() error {
	connection, err := net.DialTimeout("tcp", c.address, c.timeout)
	if err != nil {
		return fmt.Errorf("подключиться к ClamAV: %w", err)
	}
	defer func() {
		_ = connection.Close()
	}()
	_ = connection.SetDeadline(time.Now().Add(c.timeout))
	if _, err := connection.Write([]byte("zPING\x00")); err != nil {
		return fmt.Errorf("отправить ClamAV PING: %w", err)
	}
	response, err := bufio.NewReader(connection).ReadString(0)
	if err != nil {
		return fmt.Errorf("прочитать ClamAV PING: %w", err)
	}
	if strings.Trim(response, "\x00\r\n") != "PONG" {
		return fmt.Errorf("неожиданный ответ ClamAV")
	}

	return nil
}

// ScanFile потоково сканирует файл через clamd INSTREAM и не передаёт путь контейнеру.
func (c *ClamAV) ScanFile(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("открыть файл для scan: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	connection, err := net.DialTimeout("tcp", c.address, c.timeout)
	if err != nil {
		return false, fmt.Errorf("подключиться к ClamAV: %w", err)
	}
	defer func() {
		_ = connection.Close()
	}()
	_ = connection.SetDeadline(time.Now().Add(c.timeout))
	if _, err := connection.Write([]byte("zINSTREAM\x00")); err != nil {
		return false, fmt.Errorf("начать ClamAV INSTREAM: %w", err)
	}
	buffer := make([]byte, 32<<10)
	length := make([]byte, 4)
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			binary.BigEndian.PutUint32(length, uint32(count))
			if _, err := connection.Write(length); err != nil {
				return false, fmt.Errorf("отправить размер ClamAV chunk: %w", err)
			}
			if _, err := connection.Write(buffer[:count]); err != nil {
				return false, fmt.Errorf("отправить ClamAV chunk: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, fmt.Errorf("прочитать scan stream: %w", readErr)
		}
	}
	if _, err := connection.Write([]byte{0, 0, 0, 0}); err != nil {
		return false, fmt.Errorf("завершить ClamAV stream: %w", err)
	}
	response, err := bufio.NewReader(connection).ReadString(0)
	if err != nil {
		return false, fmt.Errorf("прочитать результат ClamAV: %w", err)
	}
	response = strings.Trim(response, "\x00\r\n")
	if strings.HasSuffix(response, "OK") {
		return true, nil
	}
	if strings.Contains(response, "FOUND") {
		return false, nil
	}

	return false, fmt.Errorf("ClamAV scan error")
}
