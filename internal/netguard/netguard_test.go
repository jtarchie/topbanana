package netguard

import (
	"errors"
	"net"
	"testing"
)

func TestIsBlocked(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "::1", "fc00::1", "fe80::1", "224.0.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "100.64.0.1", "100.100.100.100",
	} {
		if !IsBlocked(net.ParseIP(raw)) {
			t.Errorf("expected %s to be blocked", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946", "100.128.0.1"} {
		if IsBlocked(net.ParseIP(raw)) {
			t.Errorf("expected %s to be allowed", raw)
		}
	}
}

func TestControl(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:80", "[fe80::1]:443"} {
		err := Control("tcp", addr, nil)
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("dial %s: got %v, want ErrBlocked", addr, err)
		}
	}
	err := Control("tcp", "8.8.8.8:443", nil)
	if err != nil {
		t.Errorf("public dial: got %v, want nil", err)
	}
}
