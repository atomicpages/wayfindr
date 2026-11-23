package httpclient

import (
	"net/netip"
	"testing"
)

// TestIsPublic tests the unexported isPublic directly: it is the decision
// table of design §7.11's public-only dialer, and a table of range
// boundaries is its clearest spec (CODING_STANDARDS, Tests). Each refused
// range has its first and last address, and its neighbours where they are
// public. TestPublicOnly covers the effect through PublicOnly.
func TestIsPublic(t *testing.T) {
	for _, tt := range []struct {
		addr string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},

		// 0.0.0.0/8
		{"0.0.0.0", false},
		{"0.255.255.255", false},
		{"1.0.0.0", true},

		// 10.0.0.0/8
		{"9.255.255.255", true},
		{"10.0.0.0", false},
		{"10.255.255.255", false},
		{"11.0.0.0", true},

		// 100.64.0.0/10
		{"100.63.255.255", true},
		{"100.64.0.0", false},
		{"100.127.255.255", false},
		{"100.128.0.0", true},

		// 127.0.0.0/8
		{"126.255.255.255", true},
		{"127.0.0.0", false},
		{"127.0.0.1", false},
		{"127.255.255.255", false},
		{"128.0.0.0", true},

		// 169.254.0.0/16
		{"169.253.255.255", true},
		{"169.254.0.0", false},
		{"169.254.169.254", false},
		{"169.254.255.255", false},
		{"169.255.0.0", true},

		// 172.16.0.0/12
		{"172.15.255.255", true},
		{"172.16.0.0", false},
		{"172.31.255.255", false},
		{"172.32.0.0", true},

		// 192.0.0.0/24
		{"191.255.255.255", true},
		{"192.0.0.0", false},
		{"192.0.0.255", false},
		{"192.0.1.0", true},

		// 192.88.99.0/24
		{"192.88.98.255", true},
		{"192.88.99.0", false},
		{"192.88.99.255", false},
		{"192.88.100.0", true},

		// 192.168.0.0/16
		{"192.167.255.255", true},
		{"192.168.0.0", false},
		{"192.168.255.255", false},
		{"192.169.0.0", true},

		// 198.18.0.0/15
		{"198.17.255.255", true},
		{"198.18.0.0", false},
		{"198.19.255.255", false},
		{"198.20.0.0", true},

		// 224.0.0.0/4 (multicast), then 240.0.0.0/4
		{"223.255.255.255", true},
		{"224.0.0.0", false},
		{"239.255.255.255", false},
		{"240.0.0.0", false},
		{"255.255.255.254", false},
		{"255.255.255.255", false},

		// IPv4-mapped IPv6 is judged as its IPv4 address.
		{"::ffff:8.8.8.8", true},
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		{"::ffff:100.64.0.1", false},
		{"::ffff:169.254.169.254", false},
		{"::ffff:0.0.0.0", false},
		{"::ffff:255.255.255.255", false},

		// ::/96, which holds :: and ::1
		{"::", false},
		{"::1", false},
		{"::8.8.8.8", false},
		{"::ffff:ffff", false},

		// ::ffff:0:0:0/96 (SIIT IPv4-translated)
		{"::fffe:ffff:ffff:ffff", true},
		{"::ffff:0:0:0", false},
		{"::ffff:0:7f00:1", false},
		{"::ffff:0:ffff:ffff", false},
		{"::1:0:0:0:0", true},

		// 100::/64
		{"100::", false},
		{"100::ffff:ffff:ffff:ffff", false},
		{"100:0:0:1::", true},

		// 2001::/32
		{"2000:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"2001::", false},
		{"2001:0:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"2001:1::", true},

		// 2002::/16
		{"2001:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"2002::", false},
		{"2002:c0a8:101::1", false},
		{"2002:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"2003::", true},

		// 64:ff9b::/96
		{"64:ff9a:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"64:ff9b::", false},
		{"64:ff9b::7f00:1", false},
		{"64:ff9b::ffff:ffff", false},
		{"64:ff9b::1:0:0", true},

		// 64:ff9b:1::/48
		{"64:ff9b:0:ffff:ffff:ffff:ffff:ffff", true},
		{"64:ff9b:1::", false},
		{"64:ff9b:1:ffff:ffff:ffff:ffff:ffff", false},
		{"64:ff9b:2::", true},

		// fc00::/7 (unique local)
		{"fbff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"fc00::", false},
		{"fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"fe00::", true},

		// fe80::/10 (link-local), then fec0::/10 (site-local)
		{"fe7f:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true},
		{"fe80::", false},
		{"fe80::1%en0", false},
		{"febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
		{"fec0::", false},
		{"feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},

		// ff00::/8 (multicast)
		{"ff00::", false},
		{"ff02::1", false},
		{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},

		// A zone does not hide a refused range, nor refuse a public address.
		{"2001::1%en0", false},
		{"fc00::1%en0", false},
		{"2606:4700:4700::1111", true},
		{"2606:4700:4700::1111%en0", true},
	} {
		t.Run(tt.addr, func(t *testing.T) {
			a, err := netip.ParseAddr(tt.addr)

			if err != nil {
				t.Fatalf("parse %s: %v", tt.addr, err)
			}

			if got := isPublic(a); got != tt.want {
				t.Errorf("isPublic(%s) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}

	if isPublic(netip.Addr{}) {
		t.Error("isPublic(zero Addr) = true, want false")
	}
}
