package shared

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// NativeAccessConfig contains explicit operator-published links. These never
// enable a native listener and are not inferred from agent-reported addresses.
type NativeAccessConfig struct {
	ServerDashboardURL  string                 `json:"serverDashboardURL,omitempty"`
	ClientDashboardURLs map[string]string      `json:"clientDashboardURLs,omitempty"`
	PublishedEndpoints  []PublishedFRPEndpoint `json:"publishedEndpoints,omitempty"`
}

type PublishedFRPEndpoint struct {
	ProxyName string `json:"proxyName"`
	User      string `json:"user"`
	ClientID  string `json:"clientID"`
	URL       string `json:"url"`
}

func (c *NativeAccessConfig) Validate() error {
	if c == nil {
		return nil
	}
	invalid := errors.New("invalid monitor nativeAccess configuration")
	if c.ServerDashboardURL != "" && !nativeAccessURL(c.ServerDashboardURL, true) {
		return invalid
	}
	if len(c.ClientDashboardURLs) > 1024 || len(c.PublishedEndpoints) > 512 {
		return invalid
	}
	for id, address := range c.ClientDashboardURLs {
		n, err := strconv.ParseUint(id, 10, 63)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != id || !nativeAccessURL(address, true) {
			return invalid
		}
	}
	counts, seen := map[[3]string]int{}, map[[4]string]bool{}
	for _, entry := range c.PublishedEndpoints {
		if !detailText(entry.ProxyName, 256, false) || !detailText(entry.User, 128, true) || !detailText(entry.ClientID, 128, false) || !nativeAccessURL(entry.URL, false) {
			return invalid
		}
		key := [3]string{entry.ProxyName, entry.User, entry.ClientID}
		full := [4]string{entry.ProxyName, entry.User, entry.ClientID, entry.URL}
		counts[key]++
		if counts[key] > MaxDetailEndpoints || seen[full] {
			return invalid
		}
		seen[full] = true
	}
	return nil
}

func nativeAccessURL(raw string, dashboard bool) bool {
	if len(raw) > 2048 || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !detailHost(u.Hostname(), false) {
		return false
	}
	if strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return false
	}
	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	if dashboard {
		ip := net.ParseIP(u.Hostname())
		return u.Scheme == "https" || u.Scheme == "http" && ip != nil && ip.IsLoopback()
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return true
	}
	return detailEnum(u.Scheme, "tcp", "udp", "tcpmux") && u.Port() != "" && u.Path == ""
}
