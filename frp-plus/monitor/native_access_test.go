package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestPublishedNativeAccessIsPrivateAndDetached(t *testing.T) {
	config := testControlConfig(t, "Public node", "synthetic-node-token")
	config.NativeAccess = &shared.NativeAccessConfig{ServerDashboardURL: "https://server-dashboard.example.invalid/", ClientDashboardURLs: map[string]string{"1": "https://client-dashboard.example.invalid/"}, PublishedEndpoints: []shared.PublishedFRPEndpoint{{ProxyName: "team.web", User: "team", ClientID: "stable-client", URL: "https://web.example.invalid/"}}}
	s, err := Start(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	admin := s.adminSnapshot()
	if admin.NativeAccess.ServerDashboardURL != config.NativeAccess.ServerDashboardURL || len(admin.NativeAccess.PublishedEndpoints) != 1 {
		t.Fatal("explicit native access missing")
	}
	admin.NativeAccess.ClientDashboardURLs["1"] = "changed"
	admin.NativeAccess.PublishedEndpoints[0].URL = "changed"
	if s.nativeAccessSnapshot().ClientDashboardURLs["1"] == "changed" || config.NativeAccess.PublishedEndpoints[0].URL == "changed" {
		t.Fatal("snapshot aliases configuration")
	}
	public, err := json.Marshal(s.snapshot(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"native_access", "dashboard.example.invalid", "web.example.invalid", "stable-client"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("public access metadata leak: %s", secret)
		}
	}
}
