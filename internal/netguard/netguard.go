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

func IsBlocked(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
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
