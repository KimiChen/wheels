package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func TestNodeSettingsCalibrationVisibilityAndRevision(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	ctx := context.Background()
	m := *fixture(t, "report-first").Report.Metrics
	sample := func(rx, tx uint64) {
		m.NetRXTotal = shared.Field[uint64]{Value: &rx, Quality: shared.QualityOK}
		m.NetTXTotal = shared.Field[uint64]{Value: &tx, Quality: shared.QualityOK}
		if !s.control.Accept("1", time.Now(), m) {
			t.Fatal("sample rejected")
		}
		if err := s.control.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	sample(1000, 1000)
	sample(1100, 1080)
	n, err := s.control.Get(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	settings := nodeSettings{n.NodeConfig, n.ConfigRevision}
	patch := func(cfg nodeSettings, used *string) *http.Response {
		body := struct {
			nodeSettings
			TrafficUsedBytes *string `json:"traffic_used_bytes,omitempty"`
		}{cfg, used}
		data, _ := json.Marshal(body)
		return adminRequest(t, s, "PATCH", "/api/admin/v1/nodes/1/settings", string(data), cookie, session.CSRF, nil)
	}
	used := "150"
	expectStatus(t, patch(settings, &used), 200)
	n, _ = s.control.Get(ctx, "1")
	if n.UsedBytes() != "150" {
		t.Fatalf("calibration: %s", n.UsedBytes())
	}
	expectStatus(t, patch(settings, nil), 409)
	settings = nodeSettings{n.NodeConfig, n.ConfigRevision}
	settings.TrafficMode = "total"
	expectStatus(t, patch(settings, nil), 200)
	n, _ = s.control.Get(ctx, "1")
	if n.UsedBytes() != "150" {
		t.Fatal("mode switch changed calibrated usage")
	}
	sample(1110, 1120)
	n, _ = s.control.Get(ctx, "1")
	if n.UsedBytes() != "200" {
		t.Fatalf("continued total usage: %s", n.UsedBytes())
	}
	if s.refreshNodes(ctx) != nil {
		t.Fatal("refresh")
	}
	p := s.snapshot(time.Now()).Nodes[0]
	if p.Billing != nil || p.TrafficToday.RXBytes != "110" || p.TrafficPlan.UsedBytes != "200" {
		t.Fatalf("public independent views: %+v", p)
	}
	settings = nodeSettings{n.NodeConfig, n.ConfigRevision}
	settings.IsPublic = false
	settings.PrivateNote = "private-billing-note"
	expectStatus(t, patch(settings, nil), 200)
	if len(s.public.Load().Nodes) != 0 || len(s.adminSnapshot().Nodes) != 1 {
		t.Fatal("visibility did not take effect")
	}
	expectStatus(t, adminRequest(t, s, "GET", "/api/public/v1/nodes/1/history", "", nil, "", nil), 404)
	expectStatus(t, adminRequest(t, s, "PATCH", "/api/admin/v1/nodes/1/settings", `{"name":"A","name":"B"}`, cookie, session.CSRF, nil), 400)
	n, _ = s.control.Get(ctx, "1")
	body := `{"config_revision":` + jsonNumber(n.ConfigRevision) + `}`
	expectStatus(t, adminRequest(t, s, "POST", "/api/admin/v1/nodes/1/reset-traffic", body, cookie, session.CSRF, nil), 200)
	n, _ = s.control.Get(ctx, "1")
	if n.UsedBytes() != "0" || n.TrafficTodayRXBytes != "110" || n.CounterRXBytes == nil {
		t.Fatal("reset changed day or baseline")
	}
}

func TestAdminSnapshotWaitsForCompleteConfigurationUpdate(t *testing.T) {
	s, _, _ := testAdmin(t)
	ctx := context.Background()
	s.configMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.configMu.Unlock()
		}
	}()
	n, err := s.control.Get(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	used := "150"
	n, err = s.control.UpdateNode(ctx, "1", n.NodeConfig, &used, false, n.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.refreshNodes(ctx); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan adminSnapshot, 1)
	go func() { close(started); result <- s.adminSnapshot() }()
	<-started
	select {
	case <-result:
		t.Fatal("admin snapshot escaped an in-progress configuration update")
	case <-time.After(30 * time.Millisecond):
	}
	cfg := n.NodeConfig
	cfg.TrafficMode = "total"
	used = "300"
	n, err = s.control.UpdateNode(ctx, "1", cfg, &used, false, n.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.refreshNodes(ctx); err != nil {
		t.Fatal(err)
	}
	s.configMu.Unlock()
	locked = false
	select {
	case snap := <-result:
		if len(snap.Nodes) != 1 {
			t.Fatal(snap)
		}
		row := snap.Nodes[0]
		if row.Settings.ConfigRevision != n.ConfigRevision || row.Settings.TrafficMode != "total" || row.TrafficPlan.Mode != "total" || row.TrafficPlan.UsedBytes != "300" {
			t.Fatalf("mixed configuration revisions: %+v", row)
		}
	case <-time.After(time.Second):
		t.Fatal("admin snapshot blocked after update completed")
	}
}

func TestAmbiguousCommittedWriteFailsClosed(t *testing.T) {
	for _, writeErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			s, agent, _ := testAdmin(t)
			c := dial(t, s, agent, "ambiguous-commit")
			s.configMu.Lock()
			// The transaction really committed; model its completion racing with
			// a canceled HTTP request before the handler receives the result.
			if err := s.control.DeleteNode(context.Background(), "1"); err != nil {
				s.configMu.Unlock()
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodDelete, "/api/admin/v1/nodes/1", nil)
			s.controlWriteError(response, request, writeErr)
			s.configMu.Unlock()
			if response.Code != 503 || !s.credentialError.Load() || len(s.public.Load().Nodes) != 0 {
				t.Fatal("ambiguous commit retained public authorization", response.Code)
			}
			request.Header.Set("Authorization", "Bearer "+agent)
			if s.authenticate(request) != "" {
				t.Fatal("revoked token remained authorized")
			}
			c.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("revoked connection remained open")
			}
			s.configMu.Lock()
			s.reloadCredentials()
			s.configMu.Unlock()
			if s.credentialError.Load() || len(s.public.Load().Nodes) != 0 {
				t.Fatal("durable refresh did not restore the committed deletion")
			}
		})
	}
}

func TestInvalidWriteDoesNotRevokeUnchangedCredentials(t *testing.T) {
	s, agent, _ := testAdmin(t)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/admin/v1/nodes/1/settings", nil)
	s.configMu.Lock()
	s.controlWriteError(response, request, control.ErrInvalid)
	s.configMu.Unlock()
	request.Header.Set("Authorization", "Bearer "+agent)
	if response.Code != 400 || s.credentialError.Load() || s.authenticate(request) != "1" {
		t.Fatal("invalid input revoked valid credentials")
	}
}
func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestCommittedRevocationRefreshFailureClosesOldSession(t *testing.T) {
	s, agent, _ := testAdmin(t)
	c := dial(t, s, agent, "revoke-refresh")
	s.configMu.Lock()
	if err := s.control.DeleteNode(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.refreshCommitted(ctx)
	s.configMu.Unlock()
	if !s.credentialError.Load() || len(s.public.Load().Nodes) != 0 {
		t.Fatal("stale authorization survived failed refresh")
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("old WebSocket survived committed revocation")
	}
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	req.Header.Set("Authorization", "Bearer "+agent)
	if s.authenticate(req) != "" {
		t.Fatal("old token survived revocation")
	}
}

func TestBillingPublicAllowlistAndPreciseAmount(t *testing.T) {
	s, _, _ := testAdmin(t)
	ctx := context.Background()
	n, _ := s.control.Get(ctx, "1")
	amount, currency, cycle := "9007199254740993", "USD", "monthly"
	cfg := n.NodeConfig
	cfg.PriceMinor = &amount
	cfg.Currency = &currency
	cfg.BillingCycle = &cycle
	cfg.PrivateNote = "never-public"
	cfg.PublishBilling = true
	if _, err := s.control.UpdateNode(ctx, "1", cfg, nil, false, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	s.refreshNodes(ctx)
	data, _ := json.Marshal(s.snapshot(time.Now()))
	if !strings.Contains(string(data), `"price_minor":"9007199254740993"`) || strings.Contains(string(data), "never-public") || strings.Contains(string(data), "token_sha256") {
		t.Fatalf("billing projection invalid: %s", data)
	}
}
