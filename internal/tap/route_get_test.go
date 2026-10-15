//go:build linux && !android

package tap

import (
	"testing"
)

func TestParseRouteGet(t *testing.T) {
	tests := []struct {
		name  string
		input string
		gw    string
		dev   string
	}{
		{
			name:  "via+dev",
			input: "203.0.113.1 via 192.168.1.1 dev wlp2s0 src 192.168.1.100 uid 0\n    cache",
			gw:    "192.168.1.1",
			dev:   "wlp2s0",
		},
		{
			name:  "direct no via",
			input: "203.0.113.1 dev eth0 src 10.0.8.3 uid 0\n    cache",
			gw:    "<nil>",
			dev:   "eth0",
		},
		{
			name:  "multi path only first",
			input: "1.2.3.4 via 10.0.0.1 dev tun0 src 10.0.0.2 uid 0\n    cache",
			gw:    "10.0.0.1",
			dev:   "tun0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, dev := parseRouteGet(tt.input)
			var gws string
			if gw == nil {
				gws = "<nil>"
			} else {
				gws = gw.String()
			}
			if gws != tt.gw || dev != tt.dev {
				t.Fatalf("parseRouteGet(%q) = (%s, %s), want (%s, %s)", tt.input, gws, dev, tt.gw, tt.dev)
			}
		})
	}
}
