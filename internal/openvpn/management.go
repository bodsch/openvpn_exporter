package openvpn

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const passwordPrompt = "ENTER PASSWORD:"

// maxLineLength bounds a single line from the management interface, so a
// misbehaving peer cannot make the exporter buffer unlimited input.
const maxLineLength = 64 * 1024

func newReader(r io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(r, maxLineLength)
}

// ManagementClient queries the OpenVPN management interface.
//
// OpenVPN accepts only one management connection at a time, so every
// Fetch opens a short-lived connection and requests are serialized.
// Waiting for a running request counts against the timeout, so concurrent
// scrapes cannot pile up behind a hanging management interface.
type ManagementClient struct {
	network  string
	address  string
	password string
	timeout  time.Duration
	sem      chan struct{} // holds one token while a request is running
}

// NewManagementClient creates a client for address, which is "host:port",
// "tcp://host:port" or "unix:///path/to/socket".
func NewManagementClient(address, password string, timeout time.Duration) *ManagementClient {
	network := "tcp"
	switch {
	case strings.HasPrefix(address, "unix://"):
		network, address = "unix", strings.TrimPrefix(address, "unix://")
	case strings.HasPrefix(address, "unix:"):
		network, address = "unix", strings.TrimPrefix(address, "unix:")
	case strings.HasPrefix(address, "tcp://"):
		address = strings.TrimPrefix(address, "tcp://")
	}
	return &ManagementClient{network: network, address: address, password: password, timeout: timeout, sem: make(chan struct{}, 1)}
}

// Fetch reads "status 3" and "load-stats" from the management interface.
func (m *ManagementClient) Fetch(ctx context.Context) (*Status, *LoadStats, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("waiting for running management request: %w", ctx.Err())
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, m.network, m.address)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to management interface: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	s := &session{conn: conn, r: newReader(conn)}
	if err := s.handshake(m.password); err != nil {
		return nil, nil, err
	}

	lines, err := s.multiLine("status 3")
	if err != nil {
		return nil, nil, err
	}
	status, err := parseLines(lines)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing status: %w", err)
	}

	line, err := s.singleLine("load-stats")
	if err != nil {
		return nil, nil, err
	}
	load, err := parseLoadStats(line)
	if err != nil {
		return nil, nil, err
	}

	_ = s.send("quit")
	return status, load, nil
}

type session struct {
	conn net.Conn
	r    *bufio.Reader
}

// readLine reads one newline terminated line.
func (s *session) readLine() (string, error) {
	line, err := s.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", fmt.Errorf("reading from management interface: line longer than %d bytes", maxLineLength)
	}
	if err != nil {
		return "", fmt.Errorf("reading from management interface: %w", err)
	}
	return string(bytes.TrimRight(line, "\r\n")), nil
}

// atPasswordPrompt consumes the password prompt if it is next. The prompt is
// not terminated by a newline, so readLine would block on it. Every other
// line of the handshake is longer than the prompt, so Peek cannot wait for
// input that never comes.
func (s *session) atPasswordPrompt() bool {
	p, _ := s.r.Peek(len(passwordPrompt))
	if string(p) != passwordPrompt {
		return false
	}
	_, _ = s.r.Discard(len(p))
	return true
}

func (s *session) send(cmd string) error {
	if _, err := s.conn.Write([]byte(cmd + "\n")); err != nil {
		return fmt.Errorf("writing to management interface: %w", err)
	}
	return nil
}

// handshake authenticates (if required) and waits for the welcome banner.
func (s *session) handshake(password string) error {
	sent := false
	for {
		if s.atPasswordPrompt() {
			if password == "" {
				return errors.New("management interface requires a password, but none is configured")
			}
			if err := s.send(password); err != nil {
				return err
			}
			sent = true
			continue
		}
		line, err := s.readLine()
		if sent && errors.Is(err, io.EOF) {
			// OpenVPN closes the connection on a wrong password without an error message.
			return errors.New("management interface closed the connection after the password was sent (wrong password?)")
		}
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(line, "ERROR:"):
			return fmt.Errorf("management interface: %s", strings.TrimSpace(strings.TrimPrefix(line, "ERROR:")))
		case strings.HasPrefix(line, ">INFO:"):
			return nil
		}
	}
}

// singleLine runs a command that answers with one SUCCESS/ERROR line.
func (s *session) singleLine(cmd string) (string, error) {
	if err := s.send(cmd); err != nil {
		return "", err
	}
	for {
		line, err := s.readLine()
		if err != nil {
			return "", err
		}
		switch {
		case strings.HasPrefix(line, ">"):
			// asynchronous notification
		case strings.HasPrefix(line, "SUCCESS:"):
			return strings.TrimSpace(strings.TrimPrefix(line, "SUCCESS:")), nil
		case strings.HasPrefix(line, "ERROR:"):
			return "", fmt.Errorf("%s: %s", cmd, strings.TrimSpace(strings.TrimPrefix(line, "ERROR:")))
		}
	}
}

// multiLine runs a command whose output is terminated by "END".
func (s *session) multiLine(cmd string) ([]string, error) {
	if err := s.send(cmd); err != nil {
		return nil, err
	}
	var (
		lines []string
		size  int
	)
	for {
		line, err := s.readLine()
		if err != nil {
			return nil, err
		}
		if size += len(line) + 1; size > maxStatusSize {
			return nil, fmt.Errorf("%s: %w", cmd, errStatusTooLarge)
		}
		switch {
		case line == "END":
			return lines, nil
		case strings.HasPrefix(line, ">"):
			// asynchronous notification
		case len(lines) == 0 && strings.HasPrefix(line, "ERROR:"):
			return nil, fmt.Errorf("%s: %s", cmd, strings.TrimSpace(strings.TrimPrefix(line, "ERROR:")))
		default:
			lines = append(lines, line)
		}
	}
}

// StatusFile reads an OpenVPN status file written via the "status" option.
type StatusFile struct {
	Path string
}

// Fetch parses the status file. Load statistics are not available.
func (f StatusFile) Fetch(context.Context) (*Status, *LoadStats, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	s, err := ParseStatus(fh)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", f.Path, err)
	}
	return s, nil, nil
}
