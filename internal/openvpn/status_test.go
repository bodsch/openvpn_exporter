package openvpn

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func parseFile(t *testing.T, name string) *Status {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := ParseStatus(f)
	if err != nil {
		t.Fatalf("ParseStatus(%s): %v", name, err)
	}
	return s
}

func TestParseStatusV2V3(t *testing.T) {
	for _, name := range []string{"status-v2.log", "status-v3.log"} {
		t.Run(name, func(t *testing.T) {
			s := parseFile(t, name)

			if s.Version != "2.6.12" {
				t.Errorf("Version = %q", s.Version)
			}
			if got := s.UpdatedAt.Unix(); got != 1792404000 {
				t.Errorf("UpdatedAt = %d", got)
			}
			if len(s.Clients) != 2 {
				t.Fatalf("got %d clients", len(s.Clients))
			}
			want := Client{
				CommonName:     "filiale-01",
				RealAddress:    "203.0.113.10:51234",
				VirtualAddress: "10.8.0.6",
				Cipher:         "AES-256-GCM",
				BytesReceived:  12345,
				BytesSent:      67890,
				ConnectedSince: time.Unix(1792396800, 0),
			}
			if got := s.Clients[0]; got != want {
				t.Errorf("client[0] =\n %+v\nwant\n %+v", got, want)
			}
			if c := s.Clients[1]; c.Username != "bob" || c.VirtualIPv6Address != "fd00::10" {
				t.Errorf("client[1] = %+v", c)
			}
			if len(s.Routes) != 3 || s.Routes[1].VirtualAddress != "192.168.50.0/24" || s.Routes[1].LastRef.Unix() != 1792403995 {
				t.Errorf("routes = %+v", s.Routes)
			}
			if s.GlobalStats["Max bcast/mcast queue length"] != 2 || s.GlobalStats["dco_enabled"] != 1 {
				t.Errorf("global stats = %v", s.GlobalStats)
			}
		})
	}
}

func TestParseStatusV1(t *testing.T) {
	s := parseFile(t, "status-v1.log")

	local := func(v string) time.Time {
		tm, _ := time.ParseInLocation("2006-01-02 15:04:05", v, time.Local)
		return tm
	}
	if !s.UpdatedAt.Equal(local("2026-10-27 10:00:00")) {
		t.Errorf("UpdatedAt = %v", s.UpdatedAt)
	}
	if len(s.Clients) != 2 {
		t.Fatalf("got %d clients", len(s.Clients))
	}
	c := s.Clients[0]
	if c.CommonName != "filiale-01" || c.BytesReceived != 12345 || c.BytesSent != 67890 ||
		!c.ConnectedSince.Equal(local("2026-10-27 08:00:00")) {
		t.Errorf("client[0] = %+v", c)
	}
	if len(s.Routes) != 2 || s.Routes[0].CommonName != "filiale-01" || s.Routes[0].VirtualAddress != "10.8.0.6" {
		t.Errorf("routes = %+v", s.Routes)
	}
	if s.GlobalStats["Max bcast/mcast queue length"] != 2 {
		t.Errorf("global stats = %v", s.GlobalStats)
	}
}

func TestParseStatusOldTimeFormat(t *testing.T) {
	in := "OpenVPN CLIENT LIST\nUpdated,Tue Oct 27 10:00:00 2026\n" +
		"Common Name,Real Address,Bytes Received,Bytes Sent,Connected Since\n" +
		"a,1.2.3.4:1,1,2,Tue Oct 27 08:00:00 2026\nROUTING TABLE\nVirtual Address,Common Name,Real Address,Last Ref\nGLOBAL STATS\nEND\n"
	s, err := ParseStatus(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if s.UpdatedAt.Hour() != 10 || s.Clients[0].ConnectedSince.Hour() != 8 {
		t.Errorf("times = %v / %v", s.UpdatedAt, s.Clients[0].ConnectedSince)
	}
}

func TestParseStatusErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":   "",
		"unknown": "garbage\n",
	} {
		if _, err := ParseStatus(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseStatusSkipsBadLines(t *testing.T) {
	for name, in := range map[string]string{
		"v3": "TITLE\tx\nTIME\tnope\nHEADER\tCLIENT_LIST\tCommon Name\tBytes Received\n" +
			"CLIENT_LIST\ta\tnope\nCLIENT_LIST\tb\t42\nEND\n",
		"v1": "OpenVPN CLIENT LIST\nUpdated,nope\nCommon Name,Bytes Received\n" +
			"a,nope\nb,42\nEND\n",
	} {
		s, err := ParseStatus(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(s.Clients) != 1 || s.Clients[0].CommonName != "b" || s.Clients[0].BytesReceived != 42 {
			t.Errorf("%s: clients = %+v", name, s.Clients)
		}
		if len(s.LineErrors) != 2 {
			t.Errorf("%s: line errors = %v", name, s.LineErrors)
		}
	}
}

func TestParseLoadStats(t *testing.T) {
	ls, err := parseLoadStats("nclients=2,bytesin=123,bytesout=456")
	if err != nil {
		t.Fatal(err)
	}
	if *ls != (LoadStats{Clients: 2, BytesIn: 123, BytesOut: 456}) {
		t.Errorf("got %+v", ls)
	}
}

// repeatReader returns line forever.
type repeatReader struct{ line []byte }

func (r repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n+len(r.line) <= len(p) {
		n += copy(p[n:], r.line)
	}
	if n == 0 {
		n = copy(p, r.line)
	}
	return n, nil
}

// oversizedStatus is a valid status twice as large as maxStatusSize. It is
// finite, so that a missing limit makes the tests fail instead of hang.
func oversizedStatus() io.Reader {
	return io.MultiReader(strings.NewReader("TITLE,x\n"),
		io.LimitReader(repeatReader{[]byte("GLOBAL_STATS," + strings.Repeat("x", 4000) + ",1\n")}, 2*maxStatusSize),
		strings.NewReader("\nEND\n"))
}

func TestParseStatusTooLarge(t *testing.T) {
	if _, err := ParseStatus(oversizedStatus()); !errors.Is(err, errStatusTooLarge) {
		t.Fatalf("err = %v", err)
	}
}
