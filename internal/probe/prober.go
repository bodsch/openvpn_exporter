package probe

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// PingFunc pings a single address.
type PingFunc func(ctx context.Context, address string, opts Options) (Result, error)

// Target is an address to ping and the name it is reported under.
type Target struct {
	Name    string
	Address string
}

// Entry is the latest result for a target.
type Entry struct {
	Target
	Result
}

// Prober pings its targets in the background, so that a scrape only reads
// the latest results instead of waiting for ICMP timeouts.
type Prober struct {
	opts        Options
	every       time.Duration
	concurrency int
	ping        PingFunc
	logger      *slog.Logger
	trigger     chan struct{}

	mu      sync.Mutex
	targets map[string]string // address -> name
	results map[string]Result // address -> latest result
}

// NewProber creates a prober that pings all targets every interval using
// at most concurrency parallel pings.
func NewProber(opts Options, every time.Duration, concurrency int, ping PingFunc, logger *slog.Logger) *Prober {
	return &Prober{
		opts:        opts,
		every:       every,
		concurrency: concurrency,
		ping:        ping,
		logger:      logger,
		trigger:     make(chan struct{}, 1),
		targets:     map[string]string{},
		results:     map[string]Result{},
	}
}

// SetTargets replaces the target list. Results of removed targets are
// dropped; new targets are pinged right away instead of at the next interval.
func (p *Prober) SetTargets(targets []Target) {
	next := make(map[string]string, len(targets))
	for _, t := range targets {
		next[t.Address] = t.Name
	}

	p.mu.Lock()
	added := false
	for addr := range next {
		if _, ok := p.targets[addr]; !ok {
			added = true
		}
	}
	for addr := range p.results {
		if _, ok := next[addr]; !ok {
			delete(p.results, addr)
		}
	}
	p.targets = next
	p.mu.Unlock()

	if added {
		select {
		case p.trigger <- struct{}{}:
		default: // a round is already pending
		}
	}
}

// Results returns the latest result of every current target that has been
// pinged at least once, sorted by address.
func (p *Prober) Results() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()

	entries := make([]Entry, 0, len(p.results))
	for addr, name := range p.targets {
		if r, ok := p.results[addr]; ok {
			entries = append(entries, Entry{Target: Target{Name: name, Address: addr}, Result: r})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Address < entries[j].Address })
	return entries
}

// Run pings all targets every interval and whenever new targets appear,
// until ctx is cancelled.
func (p *Prober) Run(ctx context.Context) {
	ticker := time.NewTicker(p.every)
	defer ticker.Stop()

	for {
		p.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.trigger:
		}
	}
}

func (p *Prober) round(ctx context.Context) {
	p.mu.Lock()
	targets := make([]Target, 0, len(p.targets))
	for addr, name := range p.targets {
		targets = append(targets, Target{Name: name, Address: addr})
	}
	p.mu.Unlock()

	sem := make(chan struct{}, p.concurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			pctx, cancel := context.WithTimeout(ctx, p.opts.Timeout+time.Second)
			defer cancel()
			res, err := p.ping(pctx, t.Address, p.opts)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				p.logger.Warn("ping failed", "client", t.Name, "address", t.Address, "err", err)
			}

			p.mu.Lock()
			if _, ok := p.targets[t.Address]; ok { // still wanted
				p.results[t.Address] = res
			}
			p.mu.Unlock()
		})
	}
	wg.Wait()
}
