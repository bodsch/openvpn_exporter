// Package config loads and validates the exporter configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of the configuration file.
type Config struct {
	Servers []Server `yaml:"servers"`
}

// Server describes one OpenVPN server instance to monitor.
type Server struct {
	// Name is used as value of the "server" label.
	Name string `yaml:"name"`

	// Exactly one of Management or StatusFile must be set.
	Management *Management `yaml:"management"`
	StatusFile string      `yaml:"status_file"`

	// Certificates are PEM files (or OpenVPN configs with inline
	// <ca>/<cert> blocks) whose expiry is exported.
	Certificates []string `yaml:"certificates"`
	// CRLs are certificate revocation lists (PEM or DER) as used by crl-verify.
	CRLs []string `yaml:"crls"`

	// ExpectedClients are common names that should always be connected.
	// Missing ones are exported with openvpn_client_connections 0.
	ExpectedClients []string `yaml:"expected_clients"`

	Ping Ping `yaml:"ping"`
}

// Management describes how to reach the OpenVPN management interface.
type Management struct {
	// Address is "host:port", "tcp://host:port" or "unix:///path/to/socket".
	Address      string        `yaml:"address"`
	Password     string        `yaml:"password"`
	PasswordFile string        `yaml:"password_file"`
	Timeout      time.Duration `yaml:"timeout"`
}

// Ping configures ICMP reachability checks of VPN clients.
type Ping struct {
	// ConnectedClients pings the virtual address of every connected client.
	ConnectedClients bool         `yaml:"connected_clients"`
	Targets          []PingTarget `yaml:"targets"`

	// CheckInterval is how often the targets are pinged in the background.
	// Scrapes only read the latest results.
	CheckInterval time.Duration `yaml:"check_interval"`

	Count    int           `yaml:"count"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// Privileged uses raw sockets (needs CAP_NET_RAW) instead of
	// unprivileged ICMP sockets (needs net.ipv4.ping_group_range).
	Privileged bool `yaml:"privileged"`
	// Concurrency limits the number of parallel pings.
	Concurrency int `yaml:"concurrency"`
}

// PingTarget is a statically configured client address.
type PingTarget struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

// Enabled reports whether any ping checks are configured.
func (p Ping) Enabled() bool {
	return p.ConnectedClients || len(p.Targets) > 0
}

// Load reads, defaults and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes, defaults and validates configuration data.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) finalize() error {
	if len(c.Servers) == 0 {
		return errors.New("config: at least one server must be configured")
	}
	seen := make(map[string]bool)
	for i := range c.Servers {
		s := &c.Servers[i]
		if s.Name == "" {
			return fmt.Errorf("config: servers[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("config: duplicate server name %q", s.Name)
		}
		seen[s.Name] = true

		if (s.Management == nil) == (s.StatusFile == "") {
			return fmt.Errorf("config: server %q: exactly one of management or status_file must be set", s.Name)
		}
		if m := s.Management; m != nil {
			if m.Address == "" {
				return fmt.Errorf("config: server %q: management.address is required", s.Name)
			}
			if m.Password != "" && m.PasswordFile != "" {
				return fmt.Errorf("config: server %q: management.password and management.password_file are mutually exclusive", s.Name)
			}
			if m.PasswordFile != "" {
				pw, err := readPasswordFile(m.PasswordFile)
				if err != nil {
					return fmt.Errorf("config: server %q: %w", s.Name, err)
				}
				m.Password = pw
			}
			if m.Timeout <= 0 {
				m.Timeout = 5 * time.Second
			}
		}

		// The file path is a label value, so a duplicate would produce the
		// same series twice and fail the whole scrape.
		if err := checkUnique(s.Certificates); err != nil {
			return fmt.Errorf("config: server %q: certificates: %w", s.Name, err)
		}
		if err := checkUnique(s.CRLs); err != nil {
			return fmt.Errorf("config: server %q: crls: %w", s.Name, err)
		}

		p := &s.Ping
		if p.CheckInterval <= 0 {
			p.CheckInterval = 30 * time.Second
		}
		if p.Count <= 0 {
			p.Count = 3
		}
		if p.Interval <= 0 {
			p.Interval = 200 * time.Millisecond
		}
		if p.Timeout <= 0 {
			p.Timeout = 2 * time.Second
		}
		if p.Concurrency <= 0 {
			p.Concurrency = 16
		}
		for j, t := range p.Targets {
			if t.Name == "" || t.Address == "" {
				return fmt.Errorf("config: server %q: ping.targets[%d]: name and address are required", s.Name, j)
			}
		}
	}
	return nil
}

func checkUnique(paths []string) error {
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if seen[p] {
			return fmt.Errorf("duplicate file %q", p)
		}
		seen[p] = true
	}
	return nil
}

// readPasswordFile reads the first line of an OpenVPN management password file.
func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading password file: %w", err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return "", fmt.Errorf("password file %s is empty", path)
	}
	return line, nil
}
