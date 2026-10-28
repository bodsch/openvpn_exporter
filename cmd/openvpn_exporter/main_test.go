package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/bcrypt"

	"bodsch.me/openvpn_exporter/internal/collector"
	"bodsch.me/openvpn_exporter/internal/config"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func testRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	cfg, err := config.Parse([]byte(`
servers:
  - name: main
    status_file: ../../internal/openvpn/testdata/status-v3.log
`))
	if err != nil {
		t.Fatal(err)
	}
	return newRegistry(collector.New(cfg, discard))
}

func get(t *testing.T, url, user, password string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestHandler(t *testing.T) {
	reg := testRegistry(t)
	srv := httptest.NewServer(newHandler(reg, "/custom-metrics", 0, discard))
	defer srv.Close()

	code, body := get(t, srv.URL+"/", "", "")
	if code != http.StatusOK || !strings.Contains(body, `href="/custom-metrics"`) {
		t.Errorf("landing page: %d %q", code, body)
	}
	if code, _ := get(t, srv.URL+"/metrics", "", ""); code != http.StatusNotFound {
		t.Errorf("default path with custom telemetry path: %d, want 404", code)
	}

	code, body = get(t, srv.URL+"/custom-metrics", "", "")
	if code != http.StatusOK {
		t.Fatalf("metrics: %d %q", code, body)
	}
	for _, want := range []string{
		`openvpn_up{server="main"} 1`,
		`openvpn_connected_clients{server="main"} 2`,
		`openvpn_exporter_build_info{build_date="unknown",goversion="`,
		`go_goroutines `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// The full registry, as served, must follow the Prometheus conventions.
	problems, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "openvpn_") {
			t.Errorf("lint %s: %s", p.Metric, p.Text)
		}
	}
}

// blockingCollector holds every scrape until release is closed.
type blockingCollector struct {
	entered chan struct{}
	release chan struct{}
}

var descBlocking = prometheus.NewDesc("test_blocking", "Test metric.", nil, nil)

func (c blockingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descBlocking }
func (c blockingCollector) Collect(ch chan<- prometheus.Metric) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	ch <- prometheus.MustNewConstMetric(descBlocking, prometheus.GaugeValue, 1)
}

func TestHandlerMaxRequests(t *testing.T) {
	bc := blockingCollector{entered: make(chan struct{}, 1), release: make(chan struct{})}
	reg := prometheus.NewRegistry()
	reg.MustRegister(bc)
	srv := httptest.NewServer(newHandler(reg, "/metrics", 1, discard))
	released := false
	defer func() {
		if !released {
			close(bc.release) // let blocked scrapes finish so srv.Close returns
		}
		srv.Close()
	}()

	first := make(chan int, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			first <- 0
			return
		}
		resp.Body.Close()
		first <- resp.StatusCode
	}()
	<-bc.entered

	// Without the limit the second scrape would block too, so it gets a
	// timeout to fail instead of hanging the test.
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("second parallel scrape: %v, want 503", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second parallel scrape: %d, want 503", resp.StatusCode)
	}
	close(bc.release)
	released = true
	if code := <-first; code != http.StatusOK {
		t.Errorf("first scrape: %d, want 200", code)
	}
}

func TestServe(t *testing.T) {
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	authConfig := filepath.Join(dir, "web.yml")
	if err := os.WriteFile(authConfig, []byte("basic_auth_users:\n  prom: "+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		webConfig     string
		anonymous     int
		authenticated int
	}{
		{name: "plain", webConfig: "", anonymous: http.StatusOK, authenticated: http.StatusOK},
		{name: "basic auth", webConfig: authConfig, anonymous: http.StatusUnauthorized, authenticated: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := newServer(newHandler(testRegistry(t), "/metrics", 0, discard))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errc := make(chan error, 1)
			go func() { errc <- serve(ctx, srv, l, tt.webConfig, discard) }()

			url := "http://" + l.Addr().String() + "/metrics"
			if code, _ := get(t, url, "", ""); code != tt.anonymous {
				t.Errorf("anonymous: %d, want %d", code, tt.anonymous)
			}
			if code, _ := get(t, url, "prom", "s3cret"); code != tt.authenticated {
				t.Errorf("authenticated: %d, want %d", code, tt.authenticated)
			}
			if tt.webConfig != "" {
				if code, _ := get(t, url, "prom", "wrong"); code != http.StatusUnauthorized {
					t.Errorf("wrong password: %d, want 401", code)
				}
			}

			cancel()
			select {
			case err := <-errc:
				if err != nil {
					t.Errorf("serve after shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not return after cancel")
			}
		})
	}
}

func TestServeInvalidWebConfig(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "web.yml")
	if err := os.WriteFile(bad, []byte("tls_server_config: {cert_file: /nonexistent}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
	// If the config were ignored, serve would run until ctx expires and
	// return nil, so the test fails instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := serve(ctx, srv, l, bad, discard); err == nil {
		t.Fatal("serve accepted an invalid web config")
	}
}

// bigCollector produces a scrape of several MB, like a server with
// thousands of clients, so that a response no longer fits into the
// kernel's socket buffers.
type bigCollector struct{}

var descBig = prometheus.NewDesc("test_big", "Test metric.", []string{"label"}, nil)

func (bigCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descBig }
func (bigCollector) Collect(ch chan<- prometheus.Metric) {
	pad := strings.Repeat("x", 200)
	for i := range 20000 {
		ch <- prometheus.MustNewConstMetric(descBig, prometheus.GaugeValue, 1, fmt.Sprintf("%s-%d", pad, i))
	}
}

// TestSlowReadersDoNotBlockScrapes: clients that request /metrics and never
// read the answer must not hold the scrape slots. Otherwise three such
// connections — from anyone who can reach the port — make every
// Prometheus scrape fail with 503 for as long as they stay open.
func TestSlowReadersDoNotBlockScrapes(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(bigCollector{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(newHandler(reg, "/metrics", 1, discard))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = serve(ctx, srv, l, "", discard) }()

	for range 3 {
		conn, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("GET /metrics HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(500 * time.Millisecond) // let the slow scrapes start writing

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + l.Addr().String() + "/metrics")
	if err != nil {
		t.Fatalf("scrape next to slow readers: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("scrape next to slow readers: %d, want 200; the readers hold the scrape slot", resp.StatusCode)
	}
}

// runMain runs main in a child process with args and returns its exit code
// and output. main calls os.Exit, so it cannot run in the test process.
func runMain(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunMainHelper$")
	cmd.Env = append(os.Environ(), "OVE_MAIN_ARGS="+strings.Join(args, "\n"))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func TestRunMainHelper(t *testing.T) {
	args, ok := os.LookupEnv("OVE_MAIN_ARGS")
	if !ok {
		t.Skip("only runs as the child of runMain")
	}
	os.Args = append([]string{"openvpn_exporter"}, strings.Split(args, "\n")...)
	main()
	os.Exit(0)
}

// TestConfigCheck: the README promises that --config.check also validates
// the web config. Deployments run it before a restart; if it passed a broken
// web config, the restart would leave the exporter down until someone looks.
func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := write("config.yml", "servers:\n  - name: main\n    status_file: /run/openvpn/status.log\n")
	badCfg := write("bad-config.yml", "servers: []\n")
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	web := write("web.yml", "basic_auth_users:\n  prom: "+string(hash)+"\n")
	badWeb := write("bad-web.yml", "basic_auth_users:\n  prom: not-a-bcrypt-hash\n")

	tests := []struct {
		name string
		args []string
		exit int
	}{
		{"valid", []string{"--config.file=" + cfg, "--config.check"}, 0},
		{"valid with web config", []string{"--config.file=" + cfg, "--web.config.file=" + web, "--config.check"}, 0},
		{"invalid config", []string{"--config.file=" + badCfg, "--config.check"}, 1},
		{"invalid web config", []string{"--config.file=" + cfg, "--web.config.file=" + badWeb, "--config.check"}, 1},
		{"missing web config", []string{"--config.file=" + cfg, "--web.config.file=" + filepath.Join(dir, "nope.yml"), "--config.check"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exit, out := runMain(t, tt.args...)
			if exit != tt.exit {
				t.Errorf("exit %d, want %d; output:\n%s", exit, tt.exit, out)
			}
			if tt.exit == 0 && !strings.Contains(out, "is valid") {
				t.Errorf("output lacks confirmation:\n%s", out)
			}
		})
	}
}
