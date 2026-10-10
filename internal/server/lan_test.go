package server

import (
	"net"
	"reflect"
	"testing"
)

// ux2 (2026-10-10, real Mac): Settings → Mobile App Pairing showed a QR and
// "Server URL" of http://127.0.0.1:7654 on the desktop build, because the
// shell's window asks over loopback and the URL was derived from the request
// host. A phone scanning that connects to itself. Loopback must be swapped for
// a LAN address whenever one exists; a real hostname must be left alone.
func TestReachableHostSwapsLoopbackForLAN(t *testing.T) {
	lan := []string{"192.168.1.70", "10.0.0.5"}
	for host, want := range map[string]string{
		"127.0.0.1":        "192.168.1.70",
		"localhost":        "192.168.1.70",
		"LOCALHOST":        "192.168.1.70",
		"[::1]":            "192.168.1.70",
		"::1":              "192.168.1.70",
		"192.168.1.70":     "192.168.1.70",
		"tank.local":       "tank.local",
		"abc.nullbore.com": "abc.nullbore.com",
	} {
		if got, _ := reachableHost(host, lan); got != want {
			t.Errorf("reachableHost(%q) = %q, want %q", host, got, want)
		}
	}
	// No LAN address at all: nothing better to offer, keep loopback.
	if got, swapped := reachableHost("127.0.0.1", nil); got != "127.0.0.1" || swapped {
		t.Errorf("without LAN: got %q swapped=%v", got, swapped)
	}
	if got := reachableHostPort("127.0.0.1:7654", lan); got != "192.168.1.70:7654" {
		t.Errorf("reachableHostPort = %q", got)
	}
	if got := reachableHostPort("localhost", lan); got != "192.168.1.70" {
		t.Errorf("reachableHostPort no port = %q", got)
	}
}

func TestLanIPsFromFiltersAndOrders(t *testing.T) {
	cidr := func(s string) net.Addr {
		ip, n, _ := net.ParseCIDR(s)
		n.IP = ip
		return n
	}
	addrs := []net.Addr{
		cidr("127.0.0.1/8"),     // loopback: out
		cidr("169.254.3.4/16"),  // link-local: out
		cidr("fe80::1/64"),      // IPv6: out (phones pair over v4 here)
		cidr("203.0.113.9/24"),  // public: after private
		cidr("192.168.1.70/24"), // private
		cidr("172.17.0.1/16"),   // docker bridge, still private: listed, sorts first
		cidr("0.0.0.0/0"),       // unspecified: out
	}
	got := lanIPsFrom(addrs)
	want := []string{"172.17.0.1", "192.168.1.70", "203.0.113.9"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lanIPsFrom = %v, want %v", got, want)
	}
}

// The info endpoint carries the launch nonce the shell passed, and names the
// shell as the manager only then — a Docker server reports neither.
func TestInfoEchoesLaunchID(t *testing.T) {
	s := &Server{}
	if s.managedBy() != "" {
		t.Fatal("unmanaged server must not claim a manager")
	}
	s.LaunchID = "abc123"
	if s.managedBy() != "desktop" {
		t.Fatal("launch id set ⇒ managed_by desktop")
	}
}

// On tank the Docker bridges (172.17–172.30) sort ahead of the real LAN
// address, and the first live run handed out http://172.17.0.1:… as the
// pairing URL. The default-route interface's address must come first.
func TestOrderLANPutsDefaultRouteFirst(t *testing.T) {
	ips := []string{"172.17.0.1", "172.18.0.1", "192.168.1.66", "192.168.1.72"}
	got := orderLAN(ips, "192.168.1.66")
	want := []string{"192.168.1.66", "172.17.0.1", "172.18.0.1", "192.168.1.72"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orderLAN = %v, want %v", got, want)
	}
	if got := orderLAN(ips, "10.9.9.9"); !reflect.DeepEqual(got, ips) {
		t.Errorf("unknown preferred must leave order alone, got %v", got)
	}
	if got := orderLAN(ips, ""); !reflect.DeepEqual(got, ips) {
		t.Errorf("empty preferred must leave order alone, got %v", got)
	}
}
