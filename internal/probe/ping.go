// Package probe checks reachability of VPN clients via ICMP echo.
package probe

import (
	"context"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

// Options configures a single ping run.
type Options struct {
	Count      int
	Interval   time.Duration
	Timeout    time.Duration
	Privileged bool
}

// Result summarizes a ping run.
type Result struct {
	Sent     int
	Received int
	AvgRTT   time.Duration
}

// Ping sends opts.Count ICMP echo requests to address.
func Ping(ctx context.Context, address string, opts Options) (Result, error) {
	p, err := probing.NewPinger(address)
	if err != nil {
		return Result{}, err
	}
	p.Count = opts.Count
	p.Interval = opts.Interval
	p.Timeout = opts.Timeout
	p.SetPrivileged(opts.Privileged)

	if err := p.RunWithContext(ctx); err != nil {
		return Result{}, err
	}
	st := p.Statistics()
	return Result{Sent: st.PacketsSent, Received: st.PacketsRecv, AvgRTT: st.AvgRtt}, nil
}
