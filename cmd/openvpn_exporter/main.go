// Command openvpn_exporter exports OpenVPN server metrics for Prometheus.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		col,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:            slog.NewLogLogger(logger.Handler(), slog.LevelError),
		MaxRequestsInFlight: *maxRequests,
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html><head><title>OpenVPN Exporter</title></head><body>
<h1>OpenVPN Exporter</h1><p><a href="%s">Metrics</a></p></body></html>`, *metricsPath)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go col.Run(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting openvpn_exporter", "version", version, "build_date", buildDate, "address", *listenAddress, "servers", len(cfg.Servers))
	systemdSocket := false
	flags := &web.FlagConfig{
		WebListenAddresses: &[]string{*listenAddress},
		WebSystemdSocket:   &systemdSocket,
		WebConfigFile:      webConfigFile,
	}
	if err := web.ListenAndServe(srv, flags, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}
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
