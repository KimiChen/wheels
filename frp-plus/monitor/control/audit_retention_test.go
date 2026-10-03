package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func auditCounts(t *testing.T, s *Store) (int64, int64, int64) {
	t.Helper()
	var a, b, g int64
	if err := s.call(context.Background(), func(tx *sql.Tx) error { var err error; a, b, g, err = auditUsageTx(tx); return err }); err != nil {
		t.Fatal(err)
	}
	return a, b, g
}
func TestAuditRetentionPrunesBothHistoriesAndPreservesOperationalTargets(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	req := operationRequest(f, node.ID, 1)
	o := createOperation(t, f.s, req)
	o = transitionOperation(t, f.s, o, "cancelled", "cancelled")
	_, before, _ := auditCounts(t, f.s)
	f.clock.Add(int64(91 * 24 * time.Hour / time.Millisecond))
	r, err := f.s.PruneAudit(context.Background())
	if err != nil || r.Rows != 2 || r.Events != 2 || r.Bytes <= 0 || r.Bytes > before || r.Generation != "1" {
		t.Fatal(r, err)
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), o.OperationID)
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
	found, err := f.s.GetConfigOperation(context.Background(), o.OperationID)
	if err != nil || found.State != "cancelled" {
		t.Fatal(found, err)
	}
	item := auditRecord(t, f.s, AuditInput{Kind: "retry", ActorKind: "github", Actor: "operator", OperationID: o.OperationID, Code: "request_replayed"})
	if item.ObjectCount != 1 || item.FieldCount != 1 {
		t.Fatal("initial targets were lost", item)
	}
	page, err := f.s.ListAudit(context.Background(), AuditFilter{ObjectName: "web"}, "", 50)
	if err != nil || len(page.Items) != 1 || !page.Retention.CleanupEnabled || page.Retention.Generation != "1" {
		t.Fatal(page, err)
	}
	replay, yes, err := f.s.CreateConfigOperation(context.Background(), req)
	if err != nil || !yes || replay.State != "cancelled" {
		t.Fatal(replay, yes, err)
	}
}
func TestAuditRetentionProtectsAllActiveStatesAndPendingRestore(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	protected := []string{"draft", "validated", "prepared", "applying", "verifying", "outcome_unknown", "rolling_back", "rollback_failed"}
	ids := []string{}
	for i, state := range protected {
		node := f.create(DefaultNodeConfig(state))
		o := createOperation(t, f.s, operationRequest(f, node.ID, i+1))
		ids = append(ids, o.OperationID)
		if _, err := f.s.db.Exec("UPDATE config_operations SET state=? WHERE operation_id=?", state, o.OperationID); err != nil {
			t.Fatal(err)
		}
	}
	node := f.create(DefaultNodeConfig("restoring"))
	req := restoreRequest(node, 100)
	old := restoreOldOperation(t, f, node, req.BackupServiceID)
	req.ExpectedActiveOperationID, req.ExpectedVersion = old.OperationID, old.Version
	receipt, _, err := f.s.ClaimConfigRestore(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ConfirmConfigRestore(context.Background(), receipt.ID, receipt.Version); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(91 * 24 * time.Hour / time.Millisecond))
	if result, err := f.s.PruneAudit(context.Background()); err != nil || result.Rows != 0 || result.Events != 0 {
		t.Fatal(result, err)
	}
	for _, id := range ids {
		events, err := f.s.ListConfigOperationEvents(context.Background(), id)
		if err != nil || len(events) != 1 {
			t.Fatal(id, events, err)
		}
	}
}
func TestAuditRetentionCapacityStopsOnlyNewWorkAndKeepsReplays(t *testing.T) {
	for _, limit := range []string{"rows", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
			node := f.create(DefaultNodeConfig("node"))
			req := operationRequest(f, node.ID, 1)
			o := createOperation(t, f.s, req)
			f.s.cfg.Audit = shared.AuditConfig{RetentionDays: 90, MaxRows: 100, MaxBytes: 1 << 20}
			if limit == "bytes" {
				f.s.cfg.Audit.MaxRows = 100000
			}
			for i := 0; i < 1000; i++ {
				rows, bytes, _ := auditCounts(t, f.s)
				if rows >= f.s.cfg.Audit.MaxRows || bytes >= f.s.cfg.Audit.MaxBytes {
					break
				}
				input := auditFixtureInput()
				input.OperationID = ""
				if limit == "bytes" {
					input.Changes = nil
					for j := 0; j < 100; j++ {
						input.Changes = append(input.Changes, ConfigOperationChange{Kind: "proxy", Name: fmt.Sprintf("proxy-%d-%s", j, strings.Repeat("x", 200)), Action: "update", Fields: []string{"remotePort"}})
					}
				}
				auditRecord(t, f.s, input)
			}
			other := f.create(DefaultNodeConfig("other"))
			if _, _, err := f.s.CreateConfigOperation(context.Background(), operationRequest(f, other.ID, 2)); !errors.Is(err, ErrAuditCapacity) {
				t.Fatal("capacity admitted new operation", err)
			}
			if !f.s.Healthy() {
				t.Fatal("capacity limit marked DB unhealthy")
			}
			transitionOperation(t, f.s, o, "cancelled", "cancelled")
			got, replay, err := f.s.CreateConfigOperation(context.Background(), req)
			if err != nil || !replay || got.State != "cancelled" {
				t.Fatal(got, replay, err)
			}
			page, err := f.s.ListAudit(context.Background(), AuditFilter{}, "", 1)
			if err != nil || !page.Retention.CapacityBlocked {
				t.Fatal(page, err)
			}
		})
	}
}
func TestAuditRetentionTransactionFailureCursorAndByteBudget(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	for i := 0; i < 3; i++ {
		auditRecord(t, f.s, auditFixtureInput())
	}
	// Deliberately large persisted payloads exercise the hard batch byte budget,
	// independently of the usual per-request validator's smaller entry ceiling.
	if _, err := f.s.db.Exec("UPDATE config_audit_entries SET actor=?", strings.Repeat("x", 3<<20)); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(91 * 24 * time.Hour / time.Millisecond))
	beforeRows, beforeBytes, beforeGen := auditCounts(t, f.s)
	if _, err := f.s.db.Exec("CREATE TRIGGER reject_gc BEFORE INSERT ON config_audit_entries WHEN new.code='audit_gc' BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PruneAudit(context.Background()); err == nil {
		t.Fatal("GC audit refusal ignored")
	}
	rows, bytes, generation := auditCounts(t, f.s)
	if rows != beforeRows || bytes != beforeBytes || generation != beforeGen {
		t.Fatal("partial GC committed")
	}
	if _, err := f.s.db.Exec("DROP TRIGGER reject_gc"); err != nil {
		t.Fatal(err)
	}
	result, err := f.s.PruneAudit(context.Background())
	if err != nil || result.Rows != 2 || result.Bytes > MaxAuditExportBytes || result.Bytes < 6<<20 {
		t.Fatal(result, err)
	}
	// Repair the deliberately non-wire-safe remaining row before API checks.
	if _, err = f.s.db.Exec("UPDATE config_audit_entries SET actor='operator' WHERE actor_kind='github'"); err != nil {
		t.Fatal(err)
	}
	filter := AuditFilter{FromMS: f.clock.Load() - int64(100*24*time.Hour/time.Millisecond)}
	page, err := f.s.ListAudit(context.Background(), filter, "", 1)
	if err != nil || !page.HasMore {
		t.Fatal(page, err)
	}
	if _, err = f.s.PruneAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ListAudit(context.Background(), filter, page.NextCursor, 1); !errors.Is(err, ErrAuditCursorExpired) {
		t.Fatal("old cursor silently skipped deleted data", err)
	}
}
func TestAuditRetentionYieldsToQueuedWork(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.s.call(context.Background(), func(*sql.Tx) error { close(entered); <-release; return nil })
	}()
	<-entered
	pending := make(chan error, 1)
	go func() { pending <- f.s.Flush(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for len(f.s.queue) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, err := f.s.PruneAudit(context.Background())
	close(release)
	if !errors.Is(err, ErrAuditBusy) {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = <-pending; err != nil {
		t.Fatal(err)
	}
}

func TestAuditRetentionTTLBoundaryAndReadSnapshot(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	item := auditRecord(t, f.s, auditFixtureInput())
	f.clock.Add(int64(90 * 24 * time.Hour / time.Millisecond))
	if r, err := f.s.PruneAudit(context.Background()); err != nil || r.Rows != 0 {
		t.Fatal("TTL shortened", r, err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.s.readAuditSnapshot(context.Background(), func(tx *sql.Tx) error {
			if _, err := auditDetailTx(tx, item.AuditID); err != nil {
				return err
			}
			close(entered)
			<-release
			_, err := auditDetailTx(tx, item.AuditID)
			return err
		})
	}()
	<-entered
	f.clock.Add(1)
	r, err := f.s.PruneAudit(context.Background())
	close(release)
	if err != nil || r.Rows != 1 {
		t.Fatal(r, err)
	}
	if err = <-done; err != nil {
		t.Fatal("GC changed pinned export view", err)
	}
	if _, err = f.s.GetAudit(context.Background(), item.AuditID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
