// Command openvpn_exporter exports OpenVPN server metrics for Prometheus.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"

	"bodsch.me/openvpn_exporter/internal/collector"
	"bodsch.me/openvpn_exporter/internal/config"
)

// Set at build time via -ldflags "-X main.version=... -X main.buildDate=...".
var (
	version   = "dev"
	buildDate = "unknown"
)

func main() {
	var (
		configFile    = flag.String("config.file", "/etc/openvpn_exporter/config.yml", "Path to the configuration file.")
		configCheck   = flag.Bool("config.check", false, "Validate the configuration file and exit.")
		listenAddress = flag.String("web.listen-address", ":9176", "Address to listen on for HTTP requests.")
		metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		webConfigFile = flag.String("web.config.file", "", "Path to a web configuration file enabling TLS and/or basic authentication.")
		maxRequests   = flag.Int("web.max-requests", 3, "Maximum number of parallel scrape requests; 0 disables the limit.")
		logLevel      = flag.String("log.level", "info", "Log level: debug, info, warn or error.")
		showVersion   = flag.Bool("version", false, "Print version and exit.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("openvpn_exporter %s (built %s, %s)\n", version, buildDate, runtime.Version())
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level %q\n", *logLevel)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configFile)
	if err != nil {
		logger.Error("loading configuration failed", "file", *configFile, "err", err)
		os.Exit(1)
	}
	if err := web.Validate(*webConfigFile); err != nil {
		logger.Error("loading web configuration failed", "file", *webConfigFile, "err", err)
		os.Exit(1)
	}
	if *configCheck {
		fmt.Printf("configuration %s is valid (%d servers)\n", *configFile, len(cfg.Servers))
		return
	}

	col := collector.New(cfg, logger)
	srv := newServer(newHandler(newRegistry(col), *metricsPath, *maxRequests, logger))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go col.Run(ctx)

	l, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		logger.Error("listening failed", "address", *listenAddress, "err", err)
		os.Exit(1)
	}
	logger.Info("starting openvpn_exporter", "version", version, "build_date", buildDate, "address", l.Addr().String(), "servers", len(cfg.Servers))
	if err := serve(ctx, srv, l, *webConfigFile, logger); err != nil {
		logger.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}
}

// newServer bounds every phase of a connection, so that slow or idle
// clients cannot hold goroutines and buffered scrapes indefinitely.
// WriteTimeout covers collecting and sending a scrape.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

func newRegistry(col prometheus.Collector) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		col,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)
	return reg
}

// newHandler serves the metrics of g under metricsPath, with at most
// maxRequests scrapes collecting in parallel (0 = unlimited), and a
// landing page.
func newHandler(g prometheus.Gatherer, metricsPath string, maxRequests int, logger *slog.Logger) http.Handler {
	metrics := promhttp.HandlerFor(g, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	})
	var slots chan struct{}
	if maxRequests > 0 {
		slots = make(chan struct{}, maxRequests)
	}

	mux := http.NewServeMux()

	mux.HandleFunc(metricsPath, func(w http.ResponseWriter, r *http.Request) {
		// The slot covers collecting only. promhttp's own limit also
		// covers sending, so clients that never read the answer could
		// hold every slot and make all scrapes fail with 503.
		if slots != nil {
			select {
			case slots <- struct{}{}:
			default:
				http.Error(w, "too many concurrent scrapes", http.StatusServiceUnavailable)
				return
			}
		}
		buf := &bufferedResponse{header: w.Header()}
		metrics.ServeHTTP(buf, r)
		if slots != nil {
			<-slots
		}
		buf.sendTo(w)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html><head><title>OpenVPN Exporter</title></head><body>
<h1>OpenVPN Exporter</h1><p><a href="%s">Metrics</a></p></body></html>`, metricsPath)
	})
	return mux
}

// bufferedResponse collects a response, so that it can be sent after the
// scrape slot is released. Headers go straight to the real response.
type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}

func (b *bufferedResponse) sendTo(w http.ResponseWriter) {
	b.WriteHeader(http.StatusOK)
	w.WriteHeader(b.code)
	_, _ = b.body.WriteTo(w)
}

// serve serves srv on l until ctx is cancelled, with TLS and basic auth
// from webConfigFile if it is set. A shutdown via ctx is not an error.
func serve(ctx context.Context, srv *http.Server, l net.Listener, webConfigFile string, logger *slog.Logger) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	flags := &web.FlagConfig{WebConfigFile: &webConfigFile}
	if err := web.Serve(l, srv, flags, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func buildInfo() prometheus.Collector {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "openvpn_exporter_build_info",
		Help:        "Build information of the exporter.",
		ConstLabels: prometheus.Labels{"version": version, "build_date": buildDate, "goversion": runtime.Version()},
	})
	g.Set(1)
	return g
}
