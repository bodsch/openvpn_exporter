package probe

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProber(t *testing.T) {
	release := make(chan struct{})
	ping := func(ctx context.Context, addr string, opts Options) (Result, error) {
		if addr == "10.0.0.2" {
			// an unreachable client: blocks until the test releases it
			select {
			case <-release:
			case <-ctx.Done():
			}
			return Result{Sent: opts.Count}, nil
		}
		return Result{Sent: opts.Count, Received: opts.Count, AvgRTT: time.Millisecond}, nil
	}

	// A long interval proves that new targets are pinged right away.
	p := NewProber(Options{Count: 3, Timeout: time.Minute}, time.Hour, 4, ping, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	p.SetTargets([]Target{{Name: "a", Address: "10.0.0.1"}})
	waitFor(t, "first result", func() bool { return len(p.Results()) == 1 })

	// The round with the slow target is still running; reading results must not block.
	p.SetTargets([]Target{{Name: "a", Address: "10.0.0.1"}, {Name: "b", Address: "10.0.0.2"}})
	start := time.Now()
	res := p.Results()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Results blocked for %v", d)
	}
	if len(res) != 1 || res[0].Name != "a" || res[0].Received != 3 {
		t.Errorf("results = %+v", res)
	}

	close(release)
	waitFor(t, "slow result", func() bool { return len(p.Results()) == 2 })
	if r := p.Results()[1]; r.Address != "10.0.0.2" || r.Received != 0 || r.Sent != 3 {
		t.Errorf("slow result = %+v", r)
	}

	// Renaming keeps the result, removing drops it.
	p.SetTargets([]Target{{Name: "renamed", Address: "10.0.0.1"}})
	if res := p.Results(); len(res) != 1 || res[0].Name != "renamed" {
		t.Errorf("after removal = %+v", res)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestProberPingsOnlyNewTargets(t *testing.T) {
	var mu sync.Mutex
	count := map[string]int{}
	ping := func(_ context.Context, addr string, opts Options) (Result, error) {
		mu.Lock()
		count[addr]++
		mu.Unlock()
		return Result{Sent: opts.Count, Received: opts.Count}, nil
	}
	pinged := func(addr string) int {
		mu.Lock()
		defer mu.Unlock()
		return count[addr]
	}

	p := NewProber(Options{Count: 1, Timeout: time.Second}, time.Hour, 4, ping, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.SetTargets([]Target{{Name: "a", Address: "10.0.0.1"}})
	waitFor(t, "first result", func() bool { return len(p.Results()) == 1 })
	for i, addr := range []string{"10.0.0.2", "10.0.0.3"} {
		targets := []Target{{Name: "a", Address: "10.0.0.1"}}
		for _, a := range []string{"10.0.0.2", "10.0.0.3"}[:i+1] {
			targets = append(targets, Target{Name: a, Address: a})
		}
		p.SetTargets(targets)
		waitFor(t, addr, func() bool { return pinged(addr) == 1 })
	}

	if n := pinged("10.0.0.1"); n != 1 {
		t.Errorf("existing target pinged %d times on new targets, want 1", n)
	}
	if n := pinged("10.0.0.2"); n != 1 {
		t.Errorf("10.0.0.2 pinged %d times, want 1", n)
	}
}
