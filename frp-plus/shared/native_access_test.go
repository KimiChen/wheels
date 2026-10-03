package shared

import "testing"

func TestNativeAccessURLPolicy(t *testing.T) {
	for _, address := range []string{"https://dashboard.example.invalid/static/", "http://127.0.0.1:7500/", "http://[::1]:7400/"} {
		if !nativeAccessURL(address, true) {
			t.Errorf("valid dashboard rejected: %s", address)
		}
	}
	for _, address := range []string{"javascript:alert(1)", "//dashboard.example.invalid", "http://dashboard.example.invalid", "https://user:password@example.invalid/", "https://example.invalid/?token=private", "https://example.invalid/#secret", "https://example.invalid/%0a", "https://example.invalid:0/", "https://example.invalid:65536/", "https://example.invalid\n", "https://"} {
		if nativeAccessURL(address, true) {
			t.Errorf("unsafe dashboard accepted: %q", address)
		}
	}
	for _, address := range []string{"tcp://example.invalid:17000", "udp://[2001:db8::1]:17001", "tcpmux://example.invalid:17002", "https://example.invalid/", "http://example.invalid/app"} {
		if !nativeAccessURL(address, false) {
			t.Errorf("valid published endpoint rejected: %s", address)
		}
	}
	for _, address := range []string{"tcp://example.invalid", "udp://example.invalid:17001/path", "stcp://example.invalid:1", "file:///private/config", "http://user:secret@example.invalid"} {
		if nativeAccessURL(address, false) {
			t.Errorf("invalid endpoint accepted: %s", address)
		}
	}
}

func TestNativeAccessBindingsAndLimits(t *testing.T) {
	config := NativeAccessConfig{ClientDashboardURLs: map[string]string{"1": "https://dashboard.example.invalid/"}, PublishedEndpoints: []PublishedFRPEndpoint{{ProxyName: "team.web", User: "team", ClientID: "stable", URL: "https://web.example.invalid/"}}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.PublishedEndpoints = append(config.PublishedEndpoints, config.PublishedEndpoints[0])
	if config.Validate() == nil {
		t.Fatal("duplicate published endpoint accepted")
	}
	config.PublishedEndpoints = nil
	for _, id := range []string{"0", "01", "-1", "node", "9223372036854775808"} {
		config.ClientDashboardURLs = map[string]string{id: "https://dashboard.example.invalid"}
		if config.Validate() == nil {
			t.Errorf("invalid node ID accepted: %s", id)
		}
	}
	if (*NativeAccessConfig)(nil).Validate() != nil {
		t.Fatal("optional access requires configuration")
	}
}
