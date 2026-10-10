package server

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

func envNonEmpty(k string) bool { return os.Getenv(k) != "" }

// managedBy reports who runs this server process: "desktop" when the Tauri
// shell launched it (it passes --launch-id), else "" (Docker, a bare binary).
func (s *Server) managedBy() string {
	if s.LaunchID != "" {
		return "desktop"
	}
	return ""
}

// hasRelay reports whether a NullBore tunnel is configured, i.e. whether
// PublicURL is a hostname the whole internet can reach rather than this LAN.
func (s *Server) hasRelay() bool {
	domain, _ := s.store.GetSetting(settingRelayDomain)
	return domain != "" || envNonEmpty("NULLBORE_BASE_DOMAIN") || envNonEmpty("ABOOKIFY_PUBLIC_URL")
}

// lanURLs is http://<ip>:<port> for every LAN address of this machine, the
// addresses a phone on the same Wi-Fi can pair with. Empty when the machine
// has no non-loopback IPv4 address.
func (s *Server) lanURLs() []string {
	port := strings.TrimPrefix(s.http.Addr, ":")
	out := []string{}
	for _, ip := range lanIPs() {
		out = append(out, fmt.Sprintf("http://%s:%s", ip, port))
	}
	return out
}

// lanIPs lists this machine's non-loopback, non-link-local IPv4 addresses on
// interfaces that are up, private (RFC 1918) ones first, stable order.
func lanIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addrs []net.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		a, err := ifc.Addrs()
		if err != nil {
			continue
		}
		addrs = append(addrs, a...)
	}
	return orderLAN(lanIPsFrom(addrs), outboundIP())
}

// outboundIP is the address of the interface that carries this machine's
// default route — the one a phone on the Wi-Fi actually shares — found by
// "connecting" a UDP socket (no packet is sent). "" when there is no route.
func outboundIP() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP != nil {
		return a.IP.String()
	}
	return ""
}

// orderLAN puts preferred first when it is in ips. Docker bridges and VPN
// adapters are private addresses too and sort numerically ahead of
// 192.168.x; the default-route interface is the one worth printing first.
func orderLAN(ips []string, preferred string) []string {
	if preferred == "" {
		return ips
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip == preferred {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return ips
	}
	for _, ip := range ips {
		if ip != preferred {
			out = append(out, ip)
		}
	}
	return out
}

// lanIPsFrom is the pure part of lanIPs: the usable IPv4 addresses among
// addrs, private ones first, then the rest, each group sorted.
func lanIPsFrom(addrs []net.Addr) []string {
	var private, other []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		ip4 := ip.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
			continue
		}
		if ip4.IsPrivate() {
			private = append(private, ip4.String())
		} else {
			other = append(other, ip4.String())
		}
	}
	sort.Strings(private)
	sort.Strings(other)
	return append(private, other...)
}

// reachableHost returns host unless it is a loopback name or address AND a
// LAN address exists, in which case the first LAN address is returned (and
// true). The window the desktop shell opens asks over 127.0.0.1, so every
// pairing URL derived from the request host would point a phone at itself.
func reachableHost(host string, lan []string) (string, bool) {
	if len(lan) == 0 || !isLoopbackHost(host) {
		return host, false
	}
	return lan[0], true
}

// reachableHostPort is reachableHost for a host[:port] string, keeping the port.
func reachableHostPort(hostport string, lan []string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		h, _ := reachableHost(hostport, lan)
		return h
	}
	h, _ := reachableHost(host, lan)
	return net.JoinHostPort(h, port)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
