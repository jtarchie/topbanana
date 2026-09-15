// Package netguard is the one SSRF predicate for server-side fetches of URLs a user controls.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrBlocked marks a connection refused because the address is not on the public internet.
var ErrBlocked = errors.New("address is private, loopback, or link-local")

// cgnat is RFC 6598 shared address space: not public, and where Tailscale and some cloud private networks live; IsPrivate omits it.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func IsBlocked(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		cgnat.Contains(ip) ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// Control is a net.Dialer hook: checking the resolved address at connect time closes DNS rebinding and covers every redirect hop.
func Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("netguard: %w", err)
	}
	if IsBlocked(net.ParseIP(host)) {
		return fmt.Errorf("%s: %w", host, ErrBlocked)
	}
	return nil
}
