package ratelimit

import (
	"net"
	"testing"
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
