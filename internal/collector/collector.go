// Package collector exposes OpenVPN server state as Prometheus metrics.
package collector

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"bodsch.me/openvpn_exporter/internal/config"
	"bodsch.me/openvpn_exporter/internal/openvpn"
	"bodsch.me/openvpn_exporter/internal/probe"
	"bodsch.me/openvpn_exporter/internal/tlsfiles"
)

const ns = "openvpn"

var (
	lServer  = []string{"server"}
	lClient  = []string{"server", "common_name"}
	lInfo    = []string{"server", "common_name", "real_address", "virtual_address", "virtual_ipv6_address", "username", "cipher"}
	lCert    = []string{"server", "file", "subject", "issuer", "serial"}
	lCRL     = []string{"server", "file", "issuer"}
	lTLSFile = []string{"server", "file", "kind"}
	lPing    = []string{"server", "common_name", "address"}

	descUp            = desc("up", "Whether the OpenVPN status could be read (1) or not (0).", lServer)
	descServerInfo    = desc("server_info", "OpenVPN server version.", []string{"server", "version"})
	descUpdated       = desc("status_updated_timestamp_seconds", "Time the OpenVPN status was last updated by the server.", lServer)
	descParseErrors   = desc("status_parse_errors", "Number of lines of the last status that could not be parsed and were skipped.", lServer)
	descConnected     = desc("connected_clients", "Number of currently connected clients.", lServer)
	descMaxQueue      = desc("max_bcast_mcast_queue_length", "Maximum length of the broadcast/multicast queue.", lServer)
	descDCO           = desc("dco_enabled", "Whether data channel offload is enabled.", lServer)
	descServerRx      = desc("server_received_bytes_total", "Bytes received by the server on all links (management interface only).", lServer)
	descServerTx      = desc("server_sent_bytes_total", "Bytes sent by the server on all links (management interface only).", lServer)
	descClientConns   = desc("client_connections", "Number of active connections of a client; 0 for expected clients that are not connected.", lClient)
	descClientInfo    = desc("client_info", "Connection details of a client; the value is the number of matching connections.", lInfo)
	descClientRx      = desc("client_received_bytes_total", "Bytes received from the client, summed over its connections.", lClient)
	descClientTx      = desc("client_sent_bytes_total", "Bytes sent to the client, summed over its connections.", lClient)
	descClientSince   = desc("client_connected_since_timestamp_seconds", "Start time of the most recent connection of the client.", lClient)
	descClientLastRef = desc("client_last_activity_timestamp_seconds", "Time a route of the client was last used.", lClient)
	descCertNotAfter  = desc("certificate_not_after_timestamp_seconds", "Expiry time of the certificate.", lCert)
	descCertNotBefore = desc("certificate_not_before_timestamp_seconds", "Start of the certificate validity.", lCert)
	descCRLNext       = desc("crl_next_update_timestamp_seconds", "Time the CRL expires (nextUpdate).", lCRL)
	descCRLThis       = desc("crl_this_update_timestamp_seconds", "Time the CRL was issued (thisUpdate).", lCRL)
	descCRLRevoked    = desc("crl_revoked_certificates", "Number of certificates revoked by the CRL.", lCRL)
	descTLSFileError  = desc("tls_file_error", "Whether a certificate or CRL file could not be read (1) or not (0).", lTLSFile)
	descPingSuccess   = desc("client_ping_success", "Whether the client answered at least one ICMP echo request.", lPing)
	descPingRTT       = desc("client_ping_rtt_seconds", "Average round trip time of answered ICMP echo requests.", lPing)
	descPingLoss      = desc("client_ping_packet_loss_ratio", "Ratio of unanswered ICMP echo requests.", lPing)
)

func desc(name, help string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(ns, "", name), help, labels, nil)
}

// StatusSource provides the server status.
type StatusSource interface {
	Fetch(ctx context.Context) (*openvpn.Status, *openvpn.LoadStats, error)
}

type server struct {
	cfg    config.Server
	source StatusSource
	prober *probe.Prober // nil if no pings are configured
}

// Collector implements prometheus.Collector for a set of OpenVPN servers.
type Collector struct {
	servers []server
	tls     *tlsfiles.Cache
	logger  *slog.Logger
}

// New creates a collector for the configured servers. Ping checks only
// produce results while Run is active.
func New(cfg *config.Config, logger *slog.Logger) *Collector {
	return newCollector(cfg, logger, probe.Ping)
}

func newCollector(cfg *config.Config, logger *slog.Logger, ping probe.PingFunc) *Collector {
	c := &Collector{tls: tlsfiles.NewCache(), logger: logger}
	for _, s := range cfg.Servers {
		var src StatusSource
		if m := s.Management; m != nil {
			src = openvpn.NewManagementClient(m.Address, m.Password, m.Timeout)
		} else {
			src = openvpn.StatusFile{Path: s.StatusFile}
		}
		srv := server{cfg: s, source: src}
		if pc := s.Ping; pc.Enabled() {
			opts := probe.Options{Count: pc.Count, Interval: pc.Interval, Timeout: pc.Timeout, Privileged: pc.Privileged}
			srv.prober = probe.NewProber(opts, pc.CheckInterval, pc.Concurrency, ping, logger.With("server", s.Name))
			// Static targets are known before the first scrape.
			srv.prober.SetTargets(pingTargets(pc, nil))
		}
		c.servers = append(c.servers, srv)
	}
	return c
}

// Run pings the clients in the background until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range c.servers {
		if s.prober != nil {
			wg.Go(func() { s.prober.Run(ctx) })
		}
	}
	wg.Wait()
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descUp, descServerInfo, descUpdated, descParseErrors, descConnected, descMaxQueue, descDCO,
		descServerRx, descServerTx, descClientConns, descClientInfo, descClientRx,
		descClientTx, descClientSince, descClientLastRef, descCertNotAfter,
		descCertNotBefore, descCRLNext, descCRLThis, descCRLRevoked, descTLSFileError,
		descPingSuccess, descPingRTT, descPingLoss,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	var wg sync.WaitGroup
	for _, s := range c.servers {
		wg.Go(func() { c.collectServer(ch, s) })
	}
	wg.Wait()
}

func (c *Collector) collectServer(ch chan<- prometheus.Metric, s server) {
	name := s.cfg.Name

	status, load, err := s.source.Fetch(context.Background())
	if err != nil {
		c.logger.Warn("reading OpenVPN status failed", "server", name, "err", err)
		ch <- gauge(descUp, 0, name)
	} else {
		ch <- gauge(descUp, 1, name)
		c.collectStatus(ch, s.cfg, status, load)
	}

	c.collectTLSFiles(ch, s.cfg)

	if s.prober != nil {
		// Without a status the connected clients are unknown, so the last
		// known targets keep being pinged.
		if status != nil {
			s.prober.SetTargets(pingTargets(s.cfg.Ping, status))
		}
		collectPings(ch, name, s.prober.Results())
	}
}

type clientAgg struct {
	conns   int
	rx, tx  uint64
	since   time.Time
	lastRef time.Time
}

func (c *Collector) collectStatus(ch chan<- prometheus.Metric, cfg config.Server, st *openvpn.Status, load *openvpn.LoadStats) {
	name := cfg.Name

	if st.Version != "" {
		ch <- gauge(descServerInfo, 1, name, st.Version)
	}
	if !st.UpdatedAt.IsZero() {
		ch <- gauge(descUpdated, unix(st.UpdatedAt), name)
	}
	if n := len(st.LineErrors); n > 0 {
		c.logger.Warn("skipped unparsable status lines", "server", name, "count", n, "first", st.LineErrors[0])
	}
	ch <- gauge(descParseErrors, float64(len(st.LineErrors)), name)
	ch <- gauge(descConnected, float64(len(st.Clients)), name)
	if v, ok := st.GlobalStats["Max bcast/mcast queue length"]; ok {
		ch <- gauge(descMaxQueue, v, name)
	}
	if v, ok := st.GlobalStats["dco_enabled"]; ok {
		ch <- gauge(descDCO, v, name)
	}
	if load != nil {
		ch <- counter(descServerRx, float64(load.BytesIn), name)
		ch <- counter(descServerTx, float64(load.BytesOut), name)
	}

	// With duplicate-cn several connections share a common name, so all
	// per-client values are aggregated to keep label sets unique.
	clients := map[string]*clientAgg{}
	infos := map[[7]string]int{}
	for _, cl := range st.Clients {
		a := clients[cl.CommonName]
		if a == nil {
			a = &clientAgg{}
			clients[cl.CommonName] = a
		}
		a.conns++
		a.rx += cl.BytesReceived
		a.tx += cl.BytesSent
		if cl.ConnectedSince.After(a.since) {
			a.since = cl.ConnectedSince
		}
		infos[[7]string{name, cl.CommonName, stripPort(cl.RealAddress), cl.VirtualAddress, cl.VirtualIPv6Address, cl.Username, cl.Cipher}]++
	}
	for _, r := range st.Routes {
		if a := clients[r.CommonName]; a != nil && r.LastRef.After(a.lastRef) {
			a.lastRef = r.LastRef
		}
	}

	for cn, a := range clients {
		ch <- gauge(descClientConns, float64(a.conns), name, cn)
		ch <- counter(descClientRx, float64(a.rx), name, cn)
		ch <- counter(descClientTx, float64(a.tx), name, cn)
		if !a.since.IsZero() {
			ch <- gauge(descClientSince, unix(a.since), name, cn)
		}
		if !a.lastRef.IsZero() {
			ch <- gauge(descClientLastRef, unix(a.lastRef), name, cn)
		}
	}
	for _, cn := range cfg.ExpectedClients {
		if _, ok := clients[cn]; !ok {
			clients[cn] = &clientAgg{} // also deduplicates the config list
			ch <- gauge(descClientConns, 0, name, cn)
		}
	}
	for l, n := range infos {
		ch <- gauge(descClientInfo, float64(n), l[:]...)
	}
}

func (c *Collector) collectTLSFiles(ch chan<- prometheus.Metric, cfg config.Server) {
	name := cfg.Name

	for _, path := range cfg.Certificates {
		certs, err := c.tls.Certificates(path)
		if err != nil {
			c.logger.Warn("reading certificate failed", "server", name, "file", path, "err", err)
			ch <- gauge(descTLSFileError, 1, name, path, "certificate")
			continue
		}
		ch <- gauge(descTLSFileError, 0, name, path, "certificate")
		seen := map[[2]string]bool{}
		for _, cert := range certs {
			key := [2]string{cert.Subject, cert.Serial}
			if seen[key] {
				continue
			}
			seen[key] = true
			ch <- gauge(descCertNotAfter, unix(cert.NotAfter), name, path, cert.Subject, cert.Issuer, cert.Serial)
			ch <- gauge(descCertNotBefore, unix(cert.NotBefore), name, path, cert.Subject, cert.Issuer, cert.Serial)
		}
	}

	for _, path := range cfg.CRLs {
		crl, err := c.tls.CRL(path)
		if err != nil {
			c.logger.Warn("reading CRL failed", "server", name, "file", path, "err", err)
			ch <- gauge(descTLSFileError, 1, name, path, "crl")
			continue
		}
		ch <- gauge(descTLSFileError, 0, name, path, "crl")
		ch <- gauge(descCRLNext, unix(crl.NextUpdate), name, path, crl.Issuer)
		ch <- gauge(descCRLThis, unix(crl.ThisUpdate), name, path, crl.Issuer)
		ch <- gauge(descCRLRevoked, float64(crl.Revoked), name, path, crl.Issuer)
	}
}

// pingTargets returns the virtual addresses of the connected clients (if
// enabled) and the static targets, which win on duplicate addresses.
func pingTargets(pc config.Ping, st *openvpn.Status) []probe.Target {
	byAddr := map[string]string{}
	if pc.ConnectedClients && st != nil {
		for _, cl := range st.Clients {
			addr := cl.VirtualAddress
			if !isIP(addr) {
				addr = cl.VirtualIPv6Address
			}
			if isIP(addr) {
				byAddr[addr] = cl.CommonName
			}
		}
	}
	for _, t := range pc.Targets {
		byAddr[t.Address] = t.Name
	}

	targets := make([]probe.Target, 0, len(byAddr))
	for addr, name := range byAddr {
		targets = append(targets, probe.Target{Name: name, Address: addr})
	}
	return targets
}

func collectPings(ch chan<- prometheus.Metric, server string, entries []probe.Entry) {
	for _, e := range entries {
		success, loss := 0.0, 1.0
		if e.Sent > 0 {
			loss = float64(e.Sent-e.Received) / float64(e.Sent)
		}
		if e.Received > 0 {
			success = 1
			ch <- gauge(descPingRTT, e.AvgRTT.Seconds(), server, e.Name, e.Address)
		}
		ch <- gauge(descPingSuccess, success, server, e.Name, e.Address)
		ch <- gauge(descPingLoss, loss, server, e.Name, e.Address)
	}
}

func gauge(d *prometheus.Desc, v float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, validLabels(labels)...)
}

func counter(d *prometheus.Desc, v float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, validLabels(labels)...)
}

// validLabels replaces invalid UTF-8 in label values, which come from the
// status and certificates. MustNewConstMetric would panic on them, and the
// panic in a collectServer goroutine would crash the exporter.
func validLabels(labels []string) []string {
	for i, l := range labels {
		if !utf8.ValidString(l) {
			labels[i] = strings.ToValidUTF8(l, "\uFFFD")
		}
	}
	return labels
}

func unix(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func isIP(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// stripPort removes the protocol prefix and port from "ip:port" or
// "udp4:ip:port" to avoid a new time series on every reconnect.
func stripPort(addr string) string {
	if proto, rest, ok := strings.Cut(addr, ":"); ok && (strings.HasPrefix(proto, "udp") || strings.HasPrefix(proto, "tcp")) {
		addr = rest
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
