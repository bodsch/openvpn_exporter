package probe

import (
	"context"
	"testing"
	"time"
)

func TestPingLoopback(t *testing.T) {
	res, err := Ping(context.Background(), "127.0.0.1", Options{Count: 2, Interval: 50 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		// unprivileged ICMP needs net.ipv4.ping_group_range to include our group
		t.Skipf("ICMP not permitted: %v", err)
	}
	if res.Sent != 2 || res.Received != 2 || res.AvgRTT <= 0 {
		t.Errorf("result = %+v", res)
	}
}
