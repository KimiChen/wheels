package control

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func auditFixtureInput() AuditInput {
	return AuditInput{Kind: "preview", ActorKind: "github", Actor: "operator", NodeID: "1", ServiceID: "primary", State: "prepared", Code: "preview_available", Changes: []ConfigOperationChange{{Kind: "proxy", Name: "audit-web", Action: "update", Fields: []string{"secretKey", "remotePort"}}}, Summary: &AuditSummary{BaseRevision: strings.Repeat("a", 64), Warnings: []string{"start_filtered"}, ReloadRequired: true}}
}

func auditRecord(t *testing.T, s *Store, input AuditInput) *AuditItem {
	t.Helper()
	item, err := s.RecordAudit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestAuditMigrationFromNineBackfillsIdempotentlyAndRetainsLegacyService(t *testing.T) {
	path, db := versionEightDatabase(t)
	restores, err := schemaSection(restoreSchemaMarker, auditSchemaMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(restores + "PRAGMA user_version=9;"); err != nil {
		t.Fatal(err)
	}
	now := utc("2026-10-03T12:00:00Z").UnixMilli()
	id := configOperationID(1)
	if _, err = db.Exec("INSERT INTO nodes(id,name,token_sha256,created_at_ms,updated_at_ms) VALUES(1,'fixture',?,1,1)", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO config_operations(operation_id,node_id,service_id,base_revision,candidate_digest,creator,deadline_at_ms,idempotency_key,request_digest,state,version,created_at_ms,updated_at_ms) VALUES(?,1,'primary',?,'','operator',?,?,?,'cancelled',2,?,?)", id, strings.Repeat("a", 64), now+1000, configOperationID(2), strings.Repeat("b", 64), now, now); err != nil {
		t.Fatal(err)
	}
	changes := `[{"kind":"proxy","name":"audit-web","action":"update","fields":["secretKey"]}]`
	for i, state := range []string{"draft", "cancelled"} {
		summary := changes
		if i == 1 {
			summary = "[]"
		}
		code := state
		if i == 0 {
			code = "created"
		}
		if _, err = db.Exec("INSERT INTO config_operation_events(operation_id,version,state,code,changes_json,created_at_ms,actor) VALUES(?,?,?,?,?,?,?)", id, i+1, state, code, summary, now, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(Config{Path: path, Now: func() time.Time { return time.UnixMilli(now) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatal(version, err)
	}
	if s.MigrationBackupPath() == "" {
		t.Fatal("missing pre-migration recovery image")
	}
	page, err := s.ListAudit(context.Background(), AuditFilter{ObjectKind: "proxy", ObjectName: "audit-web"}, "", 50)
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	for _, item := range page.Items {
		if item.ServiceID != "primary" || item.PreviewSummary != nil || item.ObjectCount != 1 {
			t.Fatal(item)
		}
	}
	section, err := schemaSection(auditSchemaMarker, identitySchemaMarker)
	if err != nil {
		t.Fatal(err)
	}
	_, backfill, ok := strings.Cut(section, "-- Old events retain their original facts; unavailable previews stay NULL.")
	if !ok {
		t.Fatal("missing explicit backfill boundary")
	}
	if _, err = s.db.Exec(backfill); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM config_audit_entries").Scan(&count); err != nil || count != 2 {
		t.Fatal("duplicate backfill", count, err)
	}
}

func TestAuditMirrorsOperationAtomicallyAndKeepsTargetsOnEveryState(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("fixture"))
	ctx := context.Background()
	if _, err := f.s.db.Exec("CREATE TRIGGER deny_audit BEFORE INSERT ON config_audit_entries BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.CreateConfigOperation(ctx, operationRequest(f, node.ID, 1)); err == nil {
		t.Fatal("audit failure allowed operation")
	}
	for _, table := range []string{"config_operations", "config_operation_events", "config_audit_entries"} {
		var count int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	if _, err := f.s.db.Exec("DROP TRIGGER deny_audit"); err != nil {
		t.Fatal(err)
	}
	o := createOperation(t, f.s, operationRequest(f, node.ID, 1))
	if _, err := f.s.db.Exec("CREATE TRIGGER deny_transition BEFORE INSERT ON config_audit_entries WHEN NEW.state='cancelled' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.TransitionConfigOperation(ctx, o.OperationID, ConfigOperationTransition{ExpectedVersion: o.Version, NextState: "cancelled", Code: "cancelled"}); err == nil {
		t.Fatal("transition survived missing audit")
	}
	saved, err := f.s.GetConfigOperation(ctx, o.OperationID)
	if err != nil || saved.State != "draft" || saved.Version != 1 {
		t.Fatal(saved, err)
	}
	if _, err := f.s.db.Exec("DROP TRIGGER deny_transition"); err != nil {
		t.Fatal(err)
	}
	transitionOperation(t, f.s, o, "cancelled", "cancelled")
	if _, _, err := f.s.CreateConfigOperation(ctx, operationRequest(f, node.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.ListAudit(ctx, AuditFilter{NodeID: node.ID, ObjectKind: "proxy", ObjectName: "web"}, "", 50)
	if err != nil || len(page.Items) != 3 {
		t.Fatal("deleted node or lifecycle target lost", page, err)
	}
	if page.Items[0].Kind != "retry" || page.Items[0].Code != "request_replayed" {
		t.Fatal(page.Items)
	}
	for _, item := range page.Items {
		if item.ObjectCount != 1 || item.FieldCount != 1 {
			t.Fatal(item)
		}
	}
}

func TestAuditStableCursorFiltersAndBrowserSafeIdentifiers(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	ctx := context.Background()
	for range 5 {
		auditRecord(t, f.s, auditFixtureInput())
	}
	page, err := f.s.ListAudit(ctx, AuditFilter{}, "", 2)
	if err != nil || len(page.Items) != 2 || !page.HasMore {
		t.Fatal(page, err)
	}
	cursor := page.NextCursor
	seen := map[string]bool{}
	for _, item := range page.Items {
		seen[item.AuditID] = true
	}
	f.clock.Add(-1000)
	newer := auditRecord(t, f.s, auditFixtureInput()) // backdated insertion stays above watermark.
	for page.HasMore {
		page, err = f.s.ListAudit(ctx, AuditFilter{}, page.NextCursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.AuditID] || item.AuditID == newer.AuditID {
				t.Fatal("unstable page", item)
			}
			seen[item.AuditID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatal("skipped records", seen)
	}
	for _, filter := range []AuditFilter{{Actor: "other"}, {Kind: "export"}, {ToMS: f.clock.Load()}, {FromMS: 1, ToMS: 367 * auditDayMS}} {
		if _, err = f.s.ListAudit(ctx, filter, cursor, 2); !errors.Is(err, ErrInvalid) {
			t.Fatal("cursor/filter drift accepted", err)
		}
	}
	for _, raw := range []string{"invalid!", strings.Repeat("a", MaxAuditCursor+1), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"w":"9223372036854775808"}`))} {
		if _, err = f.s.ListAudit(ctx, AuditFilter{}, raw, 2); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err = f.s.db.Exec("UPDATE sqlite_sequence SET seq=9007199254740992 WHERE name='config_audit_entries'"); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(1000)
	item := auditRecord(t, f.s, auditFixtureInput())
	if item.AuditID != "9007199254740993" {
		t.Fatal(item.AuditID)
	}
	b, _ := json.Marshal(item)
	if !strings.Contains(string(b), `"audit_id":"9007199254740993"`) {
		t.Fatal("unsafe JS number")
	}
	for _, filter := range []AuditFilter{{ActorKind: "github", Actor: "operator"}, {ServiceID: "primary"}, {ObjectKind: "proxy", ObjectName: "audit-web"}, {State: "prepared"}, {Kind: "preview"}} {
		page, err = f.s.ListAudit(ctx, filter, "", 100)
		if err != nil || len(page.Items) != 7 {
			t.Fatal(filter, page, err)
		}
	}
}

func TestAuditRejectsUnboundedOrUnsafeSummaryAndKeepsExternalActorUnknown(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	ctx := context.Background()
	for _, change := range []func(*AuditInput){
		func(v *AuditInput) { v.Code = "native private error" },
		func(v *AuditInput) { v.Changes[0].Fields = []string{"private.raw.secret"} },
		func(v *AuditInput) { v.Summary.BaseRevision = "private password" },
		func(v *AuditInput) { v.Summary.Warnings = []string{"private path"} },
		func(v *AuditInput) { v.Actor = "" },
		func(v *AuditInput) { v.Kind = "external_drift" },
		func(v *AuditInput) { v.Changes[0].Name = "bad\nname" },
	} {
		input := auditFixtureInput()
		change(&input)
		if _, err := f.s.RecordAudit(ctx, input); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid summary accepted", err)
		}
	}
	service := configOperationID(9)
	for _, revision := range []string{strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("b", 64)} {
		if err := f.s.ObserveAuditContext(ctx, "1", service, revision, "reviewer"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.ObserveAuditContext(ctx, "1", configOperationID(10), strings.Repeat("c", 64), "reviewer"); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.ListAudit(ctx, AuditFilter{Kind: "external_drift"}, "", 50)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	item := page.Items[0]
	if item.ActorKind != "unknown" || item.Actor != "" || item.Observer != "reviewer" {
		t.Fatal("attributed a local edit to inspector", item)
	}
}

func TestAuditReadOnlySnapshotDoesNotBlockControlWritesAndIsBounded(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	auditRecord(t, f.s, auditFixtureInput())
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.s.readAuditSnapshot(context.Background(), func(tx *sql.Tx) error {
			var count int
			if err := tx.QueryRow("SELECT count(*) FROM config_audit_entries").Scan(&count); err != nil {
				return err
			}
			close(started)
			<-release
			if _, err := tx.Exec("DELETE FROM config_audit_entries"); err == nil {
				return errors.New("export connection allowed writes")
			}
			return nil
		})
	}()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := f.s.RecordAudit(ctx, auditFixtureInput()); err != nil {
		t.Fatal("reader monopolized control write queue", err)
	}
	if err := f.s.readAuditSnapshot(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrAuditBusy) {
		t.Fatal("unbounded readers", err)
	}
}

func TestAuditAgentDriftObservationIsAtomicAndDoesNotImpersonateActor(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("fixture"))
	o := createOperation(t, f.s, operationRequest(f, node.ID, 1))
	o = transitionOperation(t, f.s, o, "validated", "validated")
	o = transitionOperation(t, f.s, o, "prepared", "prepared")
	o = transitionOperation(t, f.s, o, "applying", "applying")
	view := &shared.ConfigOperationView{OperationID: o.OperationID, BaseRevision: o.BaseRevision, ContextRevision: strings.Repeat("c", 64), OldDigest: strings.Repeat("d", 64), CandidateDigest: o.CandidateDigest, State: "conflict", ErrorCode: "source_drift", CreatedAtMS: o.CreatedAtMS, UpdatedAtMS: f.clock.Load(), DeadlineAtMS: o.DeadlineAtMS}
	input := ConfigOperationTransition{ExpectedVersion: o.Version, NextState: "outcome_unknown", Code: "source_drift", Actor: "reviewer", Agent: view, AgentReceivedAtMS: f.clock.Load()}
	var err error
	o, err = f.s.TransitionConfigOperation(context.Background(), o.OperationID, input)
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedVersion = o.Version
	if _, err = f.s.TransitionConfigOperation(context.Background(), o.OperationID, input); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.ListAudit(context.Background(), AuditFilter{Kind: "external_drift", ObjectName: "web"}, "", 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].ActorKind != "unknown" || page.Items[0].Actor != "" || page.Items[0].Observer != "reviewer" {
		t.Fatal(page, err)
	}
}

func TestAuditExportHasDurableOwnAuditAndNoConfigurationValues(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	auditRecord(t, f.s, auditFixtureInput())
	out, err := f.s.ExportAudit(context.Background(), AuditFilter{}, "exporter")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out.Data)), "\n")
	if len(lines) != 2 {
		t.Fatal("export included its own newer audit", len(lines))
	}
	var header auditExportHeader
	if json.Unmarshal([]byte(lines[0]), &header) != nil || header.Kind != "audit_export" || header.WatermarkID != "1" || header.Retention.CleanupEnabled {
		t.Fatal(header)
	}
	var entry struct {
		Schema int    `json:"schema"`
		Kind   string `json:"kind"`
		AuditDetail
	}
	if json.Unmarshal([]byte(lines[1]), &entry) != nil || entry.Kind != "audit_entry" || len(entry.Changes) != 1 {
		t.Fatal(entry)
	}
	if strings.Contains(string(out.Data), "token_sha256") || strings.Contains(string(out.Data), "before\"") || strings.Contains(string(out.Data), "after\"") {
		t.Fatal("private configuration in export")
	}
	page, err := f.s.ListAudit(context.Background(), AuditFilter{Kind: "export"}, "", 50)
	if err != nil || len(page.Items) != 2 || page.Items[0].Code != "export_prepared" || page.Items[1].Code != "export_requested" {
		t.Fatal(page, err)
	}
	if page.Items[0].PreviewSummary.Rows != 1 || page.Items[0].PreviewSummary.Bytes != len(out.Data) {
		t.Fatal("wrong prepared evidence")
	}
	if _, err = f.s.db.Exec("CREATE TRIGGER deny_export BEFORE INSERT ON config_audit_entries WHEN NEW.code='export_prepared' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	if out, err = f.s.ExportAudit(context.Background(), AuditFilter{Kind: "preview"}, "exporter"); err == nil || out != nil {
		t.Fatal("export bytes escaped before prepared audit")
	}
	page, err = f.s.ListAudit(context.Background(), AuditFilter{Kind: "export"}, "", 50)
	if err != nil || page.Items[0].Code != "export_failed" {
		t.Fatal(page, err)
	}
}

func copyAuditRows(t *testing.T, s *Store, n int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare("INSERT INTO config_audit_entries(recorded_at_ms,kind,actor_kind,actor,node_id,service_id,state,code,object_count,field_count,summary_json,changes_json,origin_kind,origin_id) SELECT recorded_at_ms,kind,actor_kind,actor,node_id,service_id,state,code,object_count,field_count,summary_json,changes_json,'bounded_fixture',? FROM config_audit_entries WHERE audit_id=1")
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for i := 0; i < n; i++ {
		if _, err = statement.Exec(fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditExportRefusesRowAndByteLimitsWithoutPartialOutput(t *testing.T) {
	for _, mode := range []string{"rows", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
			input := auditFixtureInput()
			if mode == "bytes" {
				input.Changes = nil
				for i := 0; i < 180; i++ {
					input.Changes = append(input.Changes, ConfigOperationChange{Kind: "proxy", Name: strings.Repeat("x", 230) + fmt.Sprint(i), Action: "update", Fields: []string{"remotePort"}})
				}
			}
			auditRecord(t, f.s, input)
			if mode == "rows" {
				copyAuditRows(t, f.s, MaxAuditExportRows)
			} else {
				copyAuditRows(t, f.s, 180)
			}
			out, err := f.s.ExportAudit(context.Background(), AuditFilter{Kind: "preview"}, "operator")
			if !errors.Is(err, ErrAuditExportLimit) || out != nil {
				t.Fatal("partial/oversize export", err)
			}
			page, err := f.s.ListAudit(context.Background(), AuditFilter{Kind: "export"}, "", 50)
			if err != nil || len(page.Items) != 2 || page.Items[0].Code != "export_failed" {
				t.Fatal(page, err)
			}
		})
	}
}
