//go:build linux

package loxinet

import (
	"net"
	"testing"
)

func TestFcPoolLookupHostMatchesInstalledIdentity(t *testing.T) {
	cases := []struct {
		name       string
		ip         net.IP
		port       uint16
		host, want string
	}{
		{"default IPv4", net.ParseIP("172.30.99.10"), 2036, "", "172.30.99.10:2036"},
		{"default four-byte IPv4", net.IP{192, 0, 2, 4}, 443, "", "192.0.2.4:443"},
		{"explicit host preserved", net.ParseIP("172.30.99.10"), 2036, "mcp.example.com", "mcp.example.com"},
		{"explicit authority preserved", net.ParseIP("172.30.99.10"), 2036, "mcp.example.com:8443", "mcp.example.com:8443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fcPoolLookupHost(c.ip, c.port, c.host); got != c.want {
				t.Fatalf("installed pool host = %q, want %q", got, c.want)
			}
		})
	}
}
