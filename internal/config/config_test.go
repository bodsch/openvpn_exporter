package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "mgmt.pass")
	if err := os.WriteFile(pwFile, []byte("geheim\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
servers:
  - name: udp
    management:
      address: 127.0.0.1:7505
      password_file: ` + pwFile + `
  - name: tcp
    status_file: /var/log/openvpn/status-tcp.log
    ping:
      count: 5
      timeout: 3s
      targets: [{name: filiale-01, address: 10.8.0.10}]
`))
	if err != nil {
		t.Fatal(err)
	}

	udp := cfg.Servers[0]
	if udp.Management.Password != "geheim" || udp.Management.Timeout != 5*time.Second {
		t.Errorf("management = %+v", udp.Management)
	}
	if udp.Ping.Enabled() {
		t.Error("ping should be disabled without targets")
	}

	p := cfg.Servers[1].Ping
	if !p.Enabled() || p.Count != 5 || p.Timeout != 3*time.Second || p.Interval != 200*time.Millisecond || p.Concurrency != 16 || p.CheckInterval != 30*time.Second {
		t.Errorf("ping = %+v", p)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"no servers":     `servers: []`,
		"missing name":   `servers: [{status_file: /x}]`,
		"duplicate name": `servers: [{name: a, status_file: /x}, {name: a, status_file: /y}]`,
		"no source":      `servers: [{name: a}]`,
		"both sources":   `servers: [{name: a, status_file: /x, management: {address: "127.0.0.1:7505"}}]`,
		"no address":     `servers: [{name: a, management: {timeout: 1s}}]`,
		"both passwords": `servers: [{name: a, management: {address: x, password: a, password_file: /b}}]`,
		"missing pwfile": `servers: [{name: a, management: {address: x, password_file: /nonexistent}}]`,
		"unknown field":  `servers: [{name: a, status_file: /x, statusfile: /y}]`,
		"bad target":     `servers: [{name: a, status_file: /x, ping: {targets: [{name: b}]}}]`,
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(in))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "config") {
				t.Errorf("error %q lacks context", err)
			}
		})
	}
}
