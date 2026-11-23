package httpclient

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrNotPublic is what a dial made by PublicOnly's client fails with when
// the address it connects to is not public.
var ErrNotPublic = errors.New("address is not public")

// PublicOnly returns an HTTP client for fetching a URL a stranger may name.
// Its transport is a clone of the default one with no proxy (a proxy's
// address would be the one checked), and its dialer's Control refuses,
// with an error wrapping ErrNotPublic, any address that, unmapped from
// IPv4-in-IPv6, is not global unicast (loopback, link-local, multicast,
// unspecified), is private (RFC 1918, fc00::/7), or is in a refused range:
// 100.64.0.0/10 (shared), 0.0.0.0/8, 192.0.0.0/24, 192.88.99.0/24 (6to4
// relay), 198.18.0.0/15, 240.0.0.0/4 (which holds 255.255.255.255),
// ::/96 (IPv4-compatible), ::ffff:0:0:0/96 (SIIT IPv4-translated),
// 100::/64 (discard), 2001::/32 (Teredo),
// fec0::/10 (site-local), 64:ff9b::/96 and 64:ff9b:1::/48 (NAT64), or
// 2002::/16 (6to4); the translation and tunnel ranges because they embed an
// IPv4 address that may be any of the above. Control runs on the resolved
// address of every connection attempt, so a name that resolves, or
// rebinds, to such an address is refused at connect time. The client
// follows redirects as the default does and has no Timeout: callers set
// CheckRedirect on a copy and bound each request with its context
// (design §7.11).
func PublicOnly() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	// The default transport's dialer settings, plus the check.
	tr.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   refuseNonPublic,
	}).DialContext

	return &http.Client{Transport: tr}
}

// refuseNonPublic is the dialer's Control: it runs after name resolution,
// once per connection attempt, with the address about to be connected to.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)

	if err != nil {
		return fmt.Errorf("%w: unparsable address %q", ErrNotPublic, address)
	}

	if !isPublic(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrNotPublic, ap.Addr())
	}

	return nil
}

// isPublic reports whether a may be connected to: the decision table of
// design §7.11's public-only dialer.
func isPublic(a netip.Addr) bool {
	// A zone says which interface, not where: drop it so the prefixes,
	// which never match a zoned address, still apply.
	a = a.Unmap().WithZone("")

	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}

	for _, p := range refusedRanges() {
		if p.Contains(a) {
			return false
		}
	}

	return true
}

// refusedRanges are the ranges refused beyond what netip classifies:
// shared, reserved, benchmarking, and the translation and tunnel ranges
// that embed an IPv4 address.
func refusedRanges() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/96"),
		netip.MustParsePrefix("::ffff:0:0:0/96"),
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001::/32"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("fec0::/10"),
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
	}
}
