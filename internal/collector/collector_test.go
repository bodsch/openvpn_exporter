package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"bodsch.me/openvpn_exporter/internal/config"
	"bodsch.me/openvpn_exporter/internal/probe"
	"bodsch.me/openvpn_exporter/internal/tlsfiles"
)

type pingLog struct {
	mu   sync.Mutex
	addr map[string]bool
}

func (l *pingLog) pinged() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for a := range l.addr {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// newTestCollector returns a collector with a fake ping function whose
// background prober runs for the duration of the test.
func newTestCollector(t *testing.T, cfgYAML string) (*Collector, *pingLog) {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}

	log := &pingLog{addr: map[string]bool{}}
	ping := func(_ context.Context, addr string, opts probe.Options) (probe.Result, error) {
		log.mu.Lock()
		log.addr[addr] = true
		log.mu.Unlock()
		switch addr {
		case "10.8.0.6":
			return probe.Result{Sent: opts.Count, Received: opts.Count, AvgRTT: 15 * time.Millisecond}, nil
		case "10.8.0.10":
			return probe.Result{Sent: 4, Received: 1, AvgRTT: 40 * time.Millisecond}, nil
		case "10.8.0.99":
			return probe.Result{Sent: opts.Count}, nil
		}
		return probe.Result{}, errors.New("operation not permitted")
	}
	c := newCollector(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), ping)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return c, log
}

// scrapeUntilPinged scrapes (which hands the connected clients to the
// prober) until the prober has a result for want targets.
func scrapeUntilPinged(t *testing.T, c *Collector, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		testutil.CollectAndCount(c)
		if n := len(c.servers[0].prober.Results()); n == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("prober has %d results, want %d", n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCollectStatus(t *testing.T) {
	c, pinged := newTestCollector(t, `
servers:
  - name: main
    status_file: ../openvpn/testdata/status-v3.log
    expected_clients: [filiale-01, filiale-02, filiale-02]
    ping:
      connected_clients: true
      targets:
        - {name: filiale-02, address: 10.8.0.99}
        - {name: router-01, address: 10.8.0.6}
`)
	scrapeUntilPinged(t, c, 3)

	want := `
# HELP openvpn_up Whether the OpenVPN status could be read (1) or not (0).
# TYPE openvpn_up gauge
openvpn_up{server="main"} 1
# HELP openvpn_server_info OpenVPN server version.
# TYPE openvpn_server_info gauge
openvpn_server_info{server="main",version="2.6.12"} 1
# HELP openvpn_status_updated_timestamp_seconds Time the OpenVPN status was last updated by the server.
# TYPE openvpn_status_updated_timestamp_seconds gauge
openvpn_status_updated_timestamp_seconds{server="main"} 1.792404e+09
# HELP openvpn_status_parse_errors Number of lines of the last status that could not be parsed and were skipped.
# TYPE openvpn_status_parse_errors gauge
openvpn_status_parse_errors{server="main"} 0
# HELP openvpn_connected_clients Number of currently connected clients.
# TYPE openvpn_connected_clients gauge
openvpn_connected_clients{server="main"} 2
# HELP openvpn_dco_enabled Whether data channel offload is enabled.
# TYPE openvpn_dco_enabled gauge
openvpn_dco_enabled{server="main"} 1
# HELP openvpn_client_connections Number of active connections of a client; 0 for expected clients that are not connected.
# TYPE openvpn_client_connections gauge
openvpn_client_connections{common_name="filiale-01",server="main"} 1
openvpn_client_connections{common_name="filiale-02",server="main"} 0
openvpn_client_connections{common_name="laptop-bob",server="main"} 1
# HELP openvpn_client_info Connection details of a client; the value is the number of matching connections.
# TYPE openvpn_client_info gauge
openvpn_client_info{cipher="AES-256-GCM",common_name="filiale-01",real_address="203.0.113.10",server="main",username="",virtual_address="10.8.0.6",virtual_ipv6_address=""} 1
openvpn_client_info{cipher="CHACHA20-POLY1305",common_name="laptop-bob",real_address="198.51.100.7",server="main",username="bob",virtual_address="10.8.0.10",virtual_ipv6_address="fd00::10"} 1
# HELP openvpn_client_received_bytes_total Bytes received from the client, summed over its connections.
# TYPE openvpn_client_received_bytes_total counter
openvpn_client_received_bytes_total{common_name="filiale-01",server="main"} 12345
openvpn_client_received_bytes_total{common_name="laptop-bob",server="main"} 100
# HELP openvpn_client_last_activity_timestamp_seconds Time a route of the client was last used.
# TYPE openvpn_client_last_activity_timestamp_seconds gauge
openvpn_client_last_activity_timestamp_seconds{common_name="filiale-01",server="main"} 1.792403995e+09
openvpn_client_last_activity_timestamp_seconds{common_name="laptop-bob",server="main"} 1.79240388e+09
# HELP openvpn_client_ping_success Whether the client answered at least one ICMP echo request.
# TYPE openvpn_client_ping_success gauge
openvpn_client_ping_success{address="10.8.0.10",common_name="laptop-bob",server="main"} 1
openvpn_client_ping_success{address="10.8.0.6",common_name="router-01",server="main"} 1
openvpn_client_ping_success{address="10.8.0.99",common_name="filiale-02",server="main"} 0
# HELP openvpn_client_ping_packet_loss_ratio Ratio of unanswered ICMP echo requests.
# TYPE openvpn_client_ping_packet_loss_ratio gauge
openvpn_client_ping_packet_loss_ratio{address="10.8.0.10",common_name="laptop-bob",server="main"} 0.75
openvpn_client_ping_packet_loss_ratio{address="10.8.0.6",common_name="router-01",server="main"} 0
openvpn_client_ping_packet_loss_ratio{address="10.8.0.99",common_name="filiale-02",server="main"} 1
# HELP openvpn_client_ping_rtt_seconds Average round trip time of answered ICMP echo requests.
# TYPE openvpn_client_ping_rtt_seconds gauge
openvpn_client_ping_rtt_seconds{address="10.8.0.10",common_name="laptop-bob",server="main"} 0.04
openvpn_client_ping_rtt_seconds{address="10.8.0.6",common_name="router-01",server="main"} 0.015
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"openvpn_up", "openvpn_server_info", "openvpn_status_updated_timestamp_seconds",
		"openvpn_status_parse_errors", "openvpn_connected_clients", "openvpn_dco_enabled", "openvpn_client_connections",
		"openvpn_client_info", "openvpn_client_received_bytes_total",
		"openvpn_client_last_activity_timestamp_seconds", "openvpn_client_ping_success",
		"openvpn_client_ping_packet_loss_ratio", "openvpn_client_ping_rtt_seconds",
	); err != nil {
		t.Error(err)
	}
	if got := strings.Join(pinged.pinged(), ","); got != "10.8.0.10,10.8.0.6,10.8.0.99" {
		t.Errorf("pinged %s", got)
	}
	// load-stats are only available via the management interface
	if n := testutil.CollectAndCount(c, "openvpn_server_received_bytes_total"); n != 0 {
		t.Errorf("server_received_bytes_total: got %d series", n)
	}
}

func TestCollectStatusUnavailable(t *testing.T) {
	c, pinged := newTestCollector(t, `
servers:
  - name: main
    status_file: /nonexistent/status.log
    expected_clients: [filiale-01]
    ping:
      connected_clients: true
      targets: [{name: filiale-02, address: 10.8.0.99}]
`)
	want := `
# HELP openvpn_up Whether the OpenVPN status could be read (1) or not (0).
# TYPE openvpn_up gauge
openvpn_up{server="main"} 0
`
	scrapeUntilPinged(t, c, 1)
	want += `
# HELP openvpn_client_ping_success Whether the client answered at least one ICMP echo request.
# TYPE openvpn_client_ping_success gauge
openvpn_client_ping_success{address="10.8.0.99",common_name="filiale-02",server="main"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "openvpn_up", "openvpn_client_connections", "openvpn_client_ping_success"); err != nil {
		t.Error(err)
	}
	// static targets are still pinged
	if got := strings.Join(pinged.pinged(), ","); got != "10.8.0.99" {
		t.Errorf("pinged %s", got)
	}
}

func TestCollectTLSFiles(t *testing.T) {
	tf, err := tlsfiles.WriteTestFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestCollector(t, fmt.Sprintf(`
servers:
  - name: main
    status_file: ../openvpn/testdata/status-v3.log
    certificates: [%q, %q, /nonexistent.crt]
    crls: [%q]
`, tf.Server, tf.Inline, tf.CRL))

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			key := mf.GetName()
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "subject" || lp.GetName() == "kind" || (lp.GetName() == "file" && strings.HasSuffix(lp.GetValue(), "nonexistent.crt")) {
					key += "/" + lp.GetValue()
				}
			}
			if strings.Contains(key, "certificate_not_after") || strings.Contains(key, "crl_") || strings.Contains(key, "tls_file_error") {
				got[key] += m.GetGauge().GetValue()
			}
		}
	}

	expect := map[string]float64{
		"openvpn_certificate_not_after_timestamp_seconds/vpn.example.com": 2 * float64(tf.ServerNotAfter.Unix()), // server.crt + inline config
		"openvpn_certificate_not_after_timestamp_seconds/Test VPN CA":     float64(tf.CANotAfter.Unix()),
		"openvpn_crl_next_update_timestamp_seconds":                       float64(tf.CRLNextUpdate.Unix()),
		"openvpn_crl_revoked_certificates":                                1,
		"openvpn_tls_file_error/certificate":                              0,
		"openvpn_tls_file_error//nonexistent.crt/certificate":             1,
		"openvpn_tls_file_error/crl":                                      0,
	}
	for k, v := range expect {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestStripPort(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.10:51234":      "203.0.113.10",
		"udp4:203.0.113.10:51234": "203.0.113.10",
		"tcp4-server:1.2.3.4:443": "1.2.3.4",
		"[2001:db8::1]:1194":      "2001:db8::1",
		"2001:db8::1":             "2001:db8::1",
	} {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInvalidUTF8Labels(t *testing.T) {
	ch := make(chan prometheus.Metric, 1)
	ch <- gauge(descClientConns, 1, "main", "bad\xffname")
	if err := testutil.CollectAndCompare(constCollector{<-ch}, strings.NewReader(`
# HELP openvpn_client_connections Number of active connections of a client; 0 for expected clients that are not connected.
# TYPE openvpn_client_connections gauge
openvpn_client_connections{common_name="bad`+"�"+`name",server="main"} 1
`)); err != nil {
		t.Error(err)
	}
}

type constCollector []prometheus.Metric

func (c constCollector) Describe(ch chan<- *prometheus.Desc) { prometheus.DescribeByCollect(c, ch) }
func (c constCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c {
		ch <- m
	}
}
