package ratelimit

import (
	"net"
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	// 1 request/sec, burst of 2
	l := New(1, 2)
	defer l.Stop()

	// First two requests consume the burst — both allowed.
	if !l.Allow("10.0.0.1") {
		t.Fatal("first request should be allowed")
	}
	if !l.Allow("10.0.0.1") {
		t.Fatal("second request (burst) should be allowed")
	}

	// Third request exceeds burst — denied.
	if l.Allow("10.0.0.1") {
		t.Fatal("third request should be denied (burst exhausted)")
	}
}

func TestSeparateIPs(t *testing.T) {
	l := New(1, 1)
	defer l.Stop()

	// Exhaust the bucket for 10.0.0.1.
	if !l.Allow("10.0.0.1") {
		t.Fatal("first IP first request should be allowed")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("first IP second request should be denied")
	}

	// Different IP has its own bucket.
	if !l.Allow("10.0.0.2") {
		t.Fatal("second IP first request should be allowed")
	}
}

func TestCleanup(t *testing.T) {
	// Cleanup every 10ms, evict entries idle for 20ms.
	l := NewWithCleanup(1000, 1000, 10*time.Millisecond, 20*time.Millisecond)
	defer l.Stop()

	l.Allow("10.0.0.1")
	if _, ok := l.ips.Load("10.0.0.1"); !ok {
		t.Fatal("entry should exist after Allow")
	}

	// Wait long enough for the entry to become stale and be cleaned up.
	time.Sleep(100 * time.Millisecond)

	if _, ok := l.ips.Load("10.0.0.1"); ok {
		t.Fatal("stale entry should have been cleaned up")
	}
}

func TestCleanupKeepsActive(t *testing.T) {
	l := NewWithCleanup(1000, 1000, 10*time.Millisecond, 50*time.Millisecond)
	defer l.Stop()

	// Keep the entry alive by calling Allow repeatedly.
	for range 5 {
		l.Allow("10.0.0.1")
		time.Sleep(20 * time.Millisecond)
	}

	if _, ok := l.ips.Load("10.0.0.1"); !ok {
		t.Fatal("active entry should not be cleaned up")
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		want string
	}{
		{
			name: "ipv4 with port",
			addr: &net.UDPAddr{IP: net.ParseIP("192.168.1.1"), Port: 12345},
			want: "192.168.1.1",
		},
		{
			name: "ipv6 with port",
			addr: &net.UDPAddr{IP: net.ParseIP("::1"), Port: 443},
			want: "::1",
		},
		{
			name: "tcp addr",
			addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
			want: "10.0.0.1",
		},
		{
			name: "ipv6 with zone",
			addr: &net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 443, Zone: "eth0"},
			want: "fe80::1%eth0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractIP(tt.addr)
			if got != tt.want {
				t.Errorf("ExtractIP(%v) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}
