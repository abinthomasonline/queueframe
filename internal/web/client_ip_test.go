package web

import (
	"net"
	"net/http/httptest"
	"testing"

	"github.com/abinthomasonline/queueframe/internal/config"
)

func TestClientIPHonoursOnlyTrustedProxyChains(t *testing.T) {
	app := &App{config: config.Config{TrustedProxyCIDRs: []*net.IPNet{
		mustProxyCIDR(t, "172.30.255.0/24"),
		mustProxyCIDR(t, "127.0.0.0/8"),
		mustProxyCIDR(t, "::1/128"),
	}}}
	tests := []struct {
		name       string
		remoteAddr string
		forwarded  []string
		want       string
	}{
		{name: "untrusted peer cannot spoof", remoteAddr: "203.0.113.10:1234", forwarded: []string{"198.51.100.20"}, want: "203.0.113.10"},
		{name: "proxy appended client wins over spoof", remoteAddr: "172.30.255.3:443", forwarded: []string{"192.0.2.99, 198.51.100.20"}, want: "198.51.100.20"},
		{name: "trusted chain is skipped from right", remoteAddr: "172.30.255.3:443", forwarded: []string{"198.51.100.20, 172.30.255.4"}, want: "198.51.100.20"},
		{name: "multiple header lines form one chain", remoteAddr: "172.30.255.3:443", forwarded: []string{"198.51.100.20", "172.30.255.4"}, want: "198.51.100.20"},
		{name: "malformed chain fails closed", remoteAddr: "172.30.255.3:443", forwarded: []string{"198.51.100.20,not-an-ip"}, want: "172.30.255.3"},
		{name: "tailscale loopback proxy", remoteAddr: "[::1]:8443", forwarded: []string{"100.64.0.7"}, want: "100.64.0.7"},
		{name: "missing header uses socket peer", remoteAddr: "172.30.255.3:443", want: "172.30.255.3"},
		{name: "all trusted values use socket peer", remoteAddr: "172.30.255.3:443", forwarded: []string{"127.0.0.2,172.30.255.4"}, want: "172.30.255.3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "https://queueframe.example/", nil)
			request.RemoteAddr = test.remoteAddr
			for _, value := range test.forwarded {
				request.Header.Add("X-Forwarded-For", value)
			}
			if got := app.clientIP(request); got != test.want {
				t.Fatalf("clientIP() = %q, want %q", got, test.want)
			}
		})
	}
}

func mustProxyCIDR(t *testing.T, value string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		t.Fatal(err)
	}
	return network
}
