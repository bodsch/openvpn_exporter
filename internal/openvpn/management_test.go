package openvpn

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeManagement emulates the OpenVPN management interface on l.
func fakeManagement(t *testing.T, l net.Listener, password string) {
	t.Helper()
	status, err := os.ReadFile("testdata/status-v3.log")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serveManagement(conn, password, string(status))
		}
	}()
}

func serveManagement(conn net.Conn, password, status string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s)) }

	if password != "" {
		w(passwordPrompt)
		line, _ := r.ReadString('\n')
		switch strings.TrimSpace(line) {
		case password:
		case "silent":
			return // like OpenVPN 2.6: close without a message
		default:
			w("ERROR: bad password\r\n")
			return
		}
		w("SUCCESS: password is correct\r\n")
	}
	w(">INFO:OpenVPN Management Interface Version 5 -- type 'help' for more info\r\n")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch strings.TrimSpace(line) {
		case "status 3":
			// an asynchronous notification in between must be ignored
			w(">HOLD:Waiting for hold release:0\r\n")
			w(strings.ReplaceAll(status, "\n", "\r\n"))
		case "load-stats":
			w("SUCCESS: nclients=2,bytesin=1000,bytesout=2000\r\n")
		case "quit":
			return
		default:
			w("ERROR: unknown command, enter 'help' for more options\r\n")
		}
	}
}

func TestManagementClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mgmt.sock")
	unixL, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer unixL.Close()
	fakeManagement(t, unixL, "")

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpL.Close()
	fakeManagement(t, tcpL, "s3cret")

	tests := []struct {
		name, address, password string
		wantErr                 string
	}{
		{name: "unix", address: "unix://" + sock},
		{name: "tcp with password", address: "tcp://" + tcpL.Addr().String(), password: "s3cret"},
		{name: "wrong password", address: tcpL.Addr().String(), password: "wrong", wantErr: "bad password"},
		{name: "rejected password", address: tcpL.Addr().String(), password: "silent", wantErr: "wrong password?"},
		{name: "missing password", address: tcpL.Addr().String(), wantErr: "requires a password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewManagementClient(tt.address, tt.password, 2*time.Second)
			st, load, err := c.Fetch(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Clients) != 2 || st.Clients[1].CommonName != "laptop-bob" {
				t.Errorf("clients = %+v", st.Clients)
			}
			if load.BytesIn != 1000 || load.BytesOut != 2000 || load.Clients != 2 {
				t.Errorf("load = %+v", load)
			}
		})
	}
}

func TestManagementClientTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err == nil {
			defer conn.Close()
			time.Sleep(2 * time.Second) // never says anything
		}
	}()

	start := time.Now()
	_, _, err = NewManagementClient(l.Addr().String(), "", 200*time.Millisecond).Fetch(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Fetch took %v", d)
	}
}

func TestManagementClientWaitTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // never says anything
		}
	}()

	c := NewManagementClient(l.Addr().String(), "", 300*time.Millisecond)
	errs := make(chan error, 3)
	start := time.Now()
	for range 3 {
		go func() { _, _, err := c.Fetch(context.Background()); errs <- err }()
	}
	for range 3 {
		if err := <-errs; err == nil {
			t.Fatal("expected error")
		}
	}
	// Serialized without a bounded wait this would take 3 × 300ms.
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Errorf("concurrent Fetch calls took %v", d)
	}
}

// BenchmarkReadLines reads a "status 3" answer of 5000 clients.
func BenchmarkReadLines(b *testing.B) {
	var sb strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&sb, "CLIENT_LIST\tclient-%d\t203.0.113.%d:%d\t10.8.%d.%d\t\t123456789\t987654321\t2026-10-28 10:00:00\t1793181600\tUNDEF\t%d\t%d\tAES-256-GCM\r\n",
			i, i%256, 40000+i, i/256, i%256, i, i)
	}
	sb.WriteString("END\r\n")
	data := []byte(sb.String())
	b.SetBytes(int64(len(data)))

	for b.Loop() {
		s := &session{r: newReader(bytes.NewReader(data))}
		for {
			line, err := s.readLine()
			if err != nil {
				b.Fatal(err)
			}
			if line == "END" {
				break
			}
		}
	}
}

func TestReadLineTooLong(t *testing.T) {
	in := strings.Repeat("x", maxLineLength+1) + "\n"
	s := &session{r: newReader(strings.NewReader(in))}
	if _, err := s.readLine(); err == nil || !strings.Contains(err.Error(), "line longer than") {
		t.Fatalf("err = %v", err)
	}
}
