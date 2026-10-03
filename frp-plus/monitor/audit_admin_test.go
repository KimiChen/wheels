package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/control"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

const auditAdminPath = "/api/admin/v1/configuration/audit"

func decodeAuditPage(t *testing.T, response *http.Response) control.AuditPage {
	t.Helper()
	defer response.Body.Close()
	var page control.AuditPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestAuditAdminAuthenticationBoundsPaginationAndOfflineDetail(t *testing.T) {
	s, _, admin := testAdmin(t)
	cookie, session := login(t, s, admin)
	input := control.AuditInput{Kind: "preview", ActorKind: "github", Actor: "operator", NodeID: "1", ServiceID: "primary", Code: "preview_available", State: "prepared", Changes: []control.ConfigOperationChange{{Kind: "proxy", Name: "safe-object", Action: "update", Fields: []string{"secretKey"}}}}
	for range 3 {
		if _, err := s.control.RecordAudit(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	expectStatus(t, adminRequest(t, s, "GET", auditAdminPath, "", nil, "", nil), 401)
	expectStatus(t, adminRequest(t, s, "POST", auditAdminPath+"/export", `{"filters":{}}`, cookie, "", nil), 403)
	expectStatus(t, adminRequest(t, s, "POST", auditAdminPath+"/export", `{"filters":{}}`, cookie, session.CSRF, http.Header{"Origin": []string{"https://other.invalid"}}), 403)
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=2&limit=3", "?native_secret=hidden", "?cursor=" + strings.Repeat("a", 1025), "?from_ms=-1", "?from_ms=1&to_ms=40000000000", "?bad;query", "?operation_id=invalid"} {
		res := adminRequest(t, s, "GET", auditAdminPath+query, "", cookie, "", nil)
		expectStatus(t, res, 400)
		res.Body.Close()
	}
	res := adminRequest(t, s, "GET", auditAdminPath+"?limit=2&service_id=primary", "", cookie, "", nil)
	expectStatus(t, res, 200)
	page := decodeAuditPage(t, res)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == "" || page.Retention.CleanupEnabled {
		t.Fatal(page)
	}
	res = adminRequest(t, s, "GET", auditAdminPath+"?limit=2&service_id=primary&cursor="+url.QueryEscape(page.NextCursor), "", cookie, "", nil)
	expectStatus(t, res, 200)
	next := decodeAuditPage(t, res)
	if len(next.Items) != 1 || next.HasMore || next.WatermarkID != page.WatermarkID {
		t.Fatal(next)
	}
	res = adminRequest(t, s, "GET", auditAdminPath+"/"+page.Items[0].AuditID, "", cookie, "", nil)
	expectStatus(t, res, 200)
	var detail control.AuditDetail
	if json.NewDecoder(res.Body).Decode(&detail) != nil || len(detail.Changes) != 1 {
		t.Fatal(detail)
	}
	res.Body.Close()
	for _, path := range []string{"/01", "/9223372036854775808", "/1?bad;query"} {
		res = adminRequest(t, s, "GET", auditAdminPath+path, "", cookie, "", nil)
		expectStatus(t, res, 400)
		res.Body.Close()
	}
	res = adminRequest(t, s, "GET", auditAdminPath+"/99999", "", cookie, "", nil)
	expectStatus(t, res, 404)
	res.Body.Close()
	for _, body := range []string{`{}`, `{"filters":null}`, `{"filters":{},"cursor":"no"}`, `{"filters":{"secret":"hidden"}}`, `{"filters":{"from_ms":1,"from_ms":2}}`} {
		res = adminRequest(t, s, "POST", auditAdminPath+"/export", body, cookie, session.CSRF, nil)
		expectStatus(t, res, 400)
		res.Body.Close()
	}
	res = adminRequest(t, s, "POST", auditAdminPath+"/export", `{"filters":{"kind":"preview"}}`, cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	data, err := io.ReadAll(io.LimitReader(res.Body, control.MaxAuditExportBytes+1))
	res.Body.Close()
	if err != nil || strings.Count(string(data), "\n") != 4 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Fatal("invalid bounded JSONL", err)
	}
	audits, err := s.control.ListAudit(context.Background(), control.AuditFilter{Kind: "export"}, "", 50)
	if err != nil || len(audits.Items) != 2 || audits.Items[0].Code != "export_prepared" {
		t.Fatal(audits, err)
	}
	// Public HTTP does not expose the new private endpoint or ledger fields.
	res = adminRequest(t, s, "GET", "/api/public/v1/nodes", "", nil, "", nil)
	data, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(data), "audit_id") || strings.Contains(string(data), "safe-object") {
		t.Fatal("audit leaked publicly")
	}
}

func TestAuditConfigLifecyclePreviewRetryAndUnknownExternalObserver(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	input := configAdminInput()
	// The shared config fixture deliberately reuses the Service ID as a local
	// secret reference. Use an independent opaque ID for this privacy assertion.
	input.SecretValues[0].Reference = configAdminID(10)
	input.Changes[0].Secrets[0].Reference = input.SecretValues[0].Reference
	res := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil)
	expectStatus(t, res, 201)
	created := decodeConfigResponse(t, res)
	res = adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil)
	expectStatus(t, res, 200)
	res.Body.Close()
	page, err := s.control.ListAudit(context.Background(), control.AuditFilter{OperationID: created.Operation.OperationID, ObjectName: "test-tunnel"}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, item := range page.Items {
		kinds[item.Kind] = true
	}
	for _, kind := range []string{"state", "dispatch", "preview", "retry"} {
		if !kinds[kind] {
			t.Fatal("missing lifecycle", kind, kinds)
		}
	}
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), input.SecretValues[0].Value) || strings.Contains(string(data), input.SecretValues[0].Reference) {
		t.Fatal("audit retained secret or local reference")
	}
	// Inspector observes the change; it is not the actor who edited local files.
	res = adminRequest(t, s, "GET", configAdminPath, "", cookie, "", nil)
	expectStatus(t, res, 200)
	res.Body.Close()
	s.configCoordinator.mu.Lock()
	s.configCoordinator.command = func(ctx context.Context, node string, c shared.ConfigCommand) (shared.ConfigResult, error) {
		r, err := agent.handle(ctx, node, c)
		if r.Inventory != nil {
			r.Inventory.ContextRevision = strings.Repeat("e", 64)
		}
		return r, err
	}
	s.configCoordinator.mu.Unlock()
	res = adminRequest(t, s, "GET", configAdminPath, "", cookie, "", nil)
	expectStatus(t, res, 200)
	res.Body.Close()
	page, err = s.control.ListAudit(context.Background(), control.AuditFilter{Kind: "external_drift"}, "", 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].ActorKind != "unknown" || page.Items[0].Actor != "" || page.Items[0].Observer != "operator" {
		t.Fatal(page, err)
	}
}

type failingAuditWriter struct {
	header http.Header
	status int
}

func (w *failingAuditWriter) Header() http.Header    { return w.header }
func (w *failingAuditWriter) WriteHeader(status int) { w.status = status }
func (w *failingAuditWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture transport failed")
}

func TestAuditMissingStoreAndDownloadWriteFailureAreExplicit(t *testing.T) {
	empty := &Service{}
	request := httptest.NewRequest("GET", auditAdminPath, nil)
	writer := httptest.NewRecorder()
	if !empty.handleAuditAdmin(writer, request, "configuration/audit", "operator") || writer.Code != 503 {
		t.Fatal(writer.Code)
	}
	s, _, _ := testAdmin(t)
	request = httptest.NewRequest("POST", auditAdminPath+"/export", strings.NewReader(`{"filters":{}}`))
	request.Header.Set("Content-Type", "application/json")
	failed := &failingAuditWriter{header: http.Header{}}
	s.handleAuditAdmin(failed, request, "configuration/audit/export", "operator")
	page, err := s.control.ListAudit(context.Background(), control.AuditFilter{Kind: "export"}, "", 50)
	if err != nil || len(page.Items) != 3 || page.Items[0].Code != "export_failed" || page.Items[1].Code != "export_prepared" || failed.status != 200 {
		t.Fatal("delivery was not distinguished from generation", page, err)
	}
}

func TestAuditDispatchFailurePreventsAgentWrite(t *testing.T) {
	s, _, admin := testAdmin(t)
	agent := installConfigAgent(s)
	cookie, session := login(t, s, admin)
	db, err := sql.Open("sqlite", s.cfg.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TRIGGER deny_dispatch BEFORE INSERT ON config_audit_entries WHEN NEW.kind='dispatch' BEGIN SELECT RAISE(ABORT,'private database fixture error'); END"); err != nil {
		t.Fatal(err)
	}
	input := configAdminInput()
	res := adminRequest(t, s, "POST", configAdminPath+"/operations", configBody(t, input), cookie, session.CSRF, nil)
	data, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || strings.Contains(string(data), "private database fixture error") {
		t.Fatal("unsafe dispatch failure response")
	}
	var outcome configOperationResponse
	if json.Unmarshal(data, &outcome) != nil || outcome.Code != "unavailable" || outcome.Operation == nil || outcome.Operation.State != "rejected" {
		t.Fatal("audit failure did not expose a failed operation")
	}
	agent.mu.Lock()
	count := len(agent.calls)
	agent.mu.Unlock()
	if count != 0 {
		t.Fatal("Agent write preceded durable dispatch audit")
	}
}
