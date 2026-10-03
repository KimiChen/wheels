package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func configOperationID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

func operationRequest(f *fixture, node string, n int) CreateConfigOperationRequest {
	return CreateConfigOperationRequest{OperationID: configOperationID(n), NodeID: node, ServiceID: "primary", BaseRevision: strings.Repeat("a", 64), Creator: "operator", DeadlineAtMS: f.clock.Load() + int64((10*time.Minute)/time.Millisecond), IdempotencyKey: configOperationID(n + 1000), RequestDigest: fmt.Sprintf("%064x", n), Changes: []ConfigOperationChange{{Kind: "proxy", Name: "web", Action: "update", Fields: []string{"remotePort"}}}}
}

func createOperation(t *testing.T, s *Store, input CreateConfigOperationRequest) *ConfigOperation {
	t.Helper()
	o, replayed, err := s.CreateConfigOperation(context.Background(), input)
	if err != nil || replayed {
		t.Fatalf("create operation: %+v %t %v", o, replayed, err)
	}
	return o
}

func transitionOperation(t *testing.T, s *Store, previous *ConfigOperation, next, code string) *ConfigOperation {
	t.Helper()
	input := ConfigOperationTransition{ExpectedVersion: previous.Version, NextState: next, Code: code, CandidateDigest: previous.CandidateDigest}
	if next == "validated" {
		input.CandidateDigest = strings.Repeat("b", 64)
	}
	o, err := s.TransitionConfigOperation(context.Background(), previous.OperationID, input)
	if err != nil {
		t.Fatalf("%s -> %s: %v", previous.State, next, err)
	}
	return o
}

func TestConfigOperationIdempotencyAndDatabaseActiveUniqueness(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	input := operationRequest(f, node.ID, 1)
	created := createOperation(t, f.s, input)
	input.OperationID = configOperationID(99)
	replayed, isReplay, err := f.s.CreateConfigOperation(context.Background(), input)
	if err != nil || !isReplay || !reflect.DeepEqual(replayed, created) {
		t.Fatal("idempotency did not retain the original operation", replayed, isReplay, err)
	}
	input.RequestDigest = strings.Repeat("c", 64)
	if _, _, err := f.s.CreateConfigOperation(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatal("idempotency key reused for another request", err)
	}
	if _, _, err := f.s.CreateConfigOperation(context.Background(), operationRequest(f, node.ID, 2)); !errors.Is(err, ErrConflict) {
		t.Fatal("multiple active node operations accepted", err)
	}
	// Exercise the DB invariant directly, independently of application checks.
	if _, err := f.s.db.Exec("INSERT INTO config_operations SELECT ?,node_id,'other-service',base_revision,candidate_digest,creator,deadline_at_ms,?,request_digest,state,version,created_at_ms,updated_at_ms,agent_result_json,agent_observed_at_ms FROM config_operations WHERE operation_id=?", configOperationID(3), configOperationID(1003), created.OperationID); err == nil {
		t.Fatal("database lacks active-operation uniqueness")
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), created.OperationID)
	if err != nil || len(events) != 1 || events[0].Code != "created" || events[0].Version != 1 {
		t.Fatal("replay wrote duplicate audit events", events, err)
	}
	transitionOperation(t, f.s, created, "cancelled", "cancelled")
	createOperation(t, f.s, operationRequest(f, node.ID, 4))
}

func TestConfigOperationDatabaseActiveStatesMatchProtocol(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	o := createOperation(t, f.s, operationRequest(f, node.ID, 1))
	for _, state := range []string{"draft", "validated", "prepared", "applying", "verifying", "confirmed", "rejected", "conflict", "failed", "outcome_unknown", "cancelled", "rolling_back", "rolled_back", "rollback_failed"} {
		if _, err := f.s.db.Exec("UPDATE config_operations SET state=? WHERE operation_id=?", state, o.OperationID); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.db.Exec("INSERT INTO config_operations SELECT ?,node_id,'other-service',base_revision,candidate_digest,creator,deadline_at_ms,?,request_digest,'draft',1,created_at_ms,updated_at_ms,agent_result_json,agent_observed_at_ms FROM config_operations WHERE operation_id=?", configOperationID(2), configOperationID(1002), o.OperationID)
		if shared.ConfigOperationActive(state) {
			if err == nil {
				t.Fatalf("active state %s did not reserve the node in SQLite", state)
			}
		} else {
			if err != nil {
				t.Fatalf("terminal state %s retained the node in SQLite: %v", state, err)
			}
			if _, err := f.s.db.Exec("DELETE FROM config_operations WHERE operation_id=?", configOperationID(2)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestConfigOperationLostPrepareCanReconcileExpiredRecovery(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	o := createOperation(t, f.s, operationRequest(f, node.ID, 1))
	o = transitionOperation(t, f.s, o, "outcome_unknown", "connection_lost")
	if o.CandidateDigest != "" {
		t.Fatal("lost prepare unexpectedly had a digest")
	}
	f.clock.Store(o.DeadlineAtMS + 1)
	recovered, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{
		ExpectedVersion: o.Version, NextState: "rolled_back", Code: "recovered", CandidateDigest: strings.Repeat("b", 64),
	})
	if err != nil || recovered.State != "rolled_back" || recovered.CandidateDigest != strings.Repeat("b", 64) {
		t.Fatal("cannot record actual post-deadline Agent recovery", recovered, err)
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), o.OperationID)
	if err != nil || len(events) != 3 || events[2].Code != "recovered" {
		t.Fatal("recovery not audited", err)
	}
	createOperation(t, f.s, operationRequest(f, node.ID, 2))
}

func TestConfigOperationConcurrentCreationHasSingleWinner(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	for _, n := range []int{1, 2} {
		input := operationRequest(f, node.ID, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := f.s.CreateConfigOperation(context.Background(), input)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("concurrent creates did not serialize", wins, conflicts)
	}
}

func TestConfigOperationStateCASAuditAtomicityAndDigest(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	o := createOperation(t, f.s, operationRequest(f, node.ID, 1))
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: 1, NextState: "confirmed", Code: "confirmed"}); !errors.Is(err, ErrConflict) {
		t.Fatal("invalid state jump accepted", err)
	}
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: 1, NextState: "validated", Code: "validated"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("validated state has no candidate digest", err)
	}
	if _, err := f.s.db.Exec("CREATE TRIGGER fail_config_audit BEFORE INSERT ON config_operation_events WHEN NEW.version=2 BEGIN SELECT RAISE(ABORT,'synthetic audit failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: 1, NextState: "validated", Code: "validated", CandidateDigest: strings.Repeat("b", 64)}); err == nil {
		t.Fatal("audit failure did not reject the transition")
	}
	after, err := f.s.GetConfigOperation(context.Background(), o.OperationID)
	if err != nil || !reflect.DeepEqual(after, o) {
		t.Fatal("failed audit partially changed operation or digest", after, err)
	}
	if _, err := f.s.db.Exec("DROP TRIGGER fail_config_audit"); err != nil {
		t.Fatal(err)
	}
	o = transitionOperation(t, f.s, o, "validated", "validated")
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: 1, NextState: "prepared", Code: "prepared"}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale operation version accepted", err)
	}
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: o.Version, NextState: "prepared", Code: "prepared", CandidateDigest: strings.Repeat("c", 64)}); !errors.Is(err, ErrConflict) {
		t.Fatal("validated candidate digest was changed", err)
	}
	for _, step := range [][2]string{{"prepared", "prepared"}, {"applying", "applying"}, {"verifying", "verifying"}, {"confirmed", "confirmed"}} {
		o = transitionOperation(t, f.s, o, step[0], step[1])
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), o.OperationID)
	if err != nil || len(events) != 6 {
		t.Fatal("state/audit history not atomic", events, err)
	}
	for i, e := range events {
		if e.Version != int64(i+1) || e.OperationID != o.OperationID {
			t.Fatal("audit sequence gap", e)
		}
	}
	confirmed := o
	next := createOperation(t, f.s, operationRequest(f, node.ID, 2))
	if _, err := f.s.TransitionConfigOperation(context.Background(), confirmed.OperationID, ConfigOperationTransition{ExpectedVersion: confirmed.Version, NextState: "rolling_back", Code: "rolling_back"}); !errors.Is(err, ErrConflict) {
		t.Fatal("explicit rollback bypassed another active operation", err)
	}
	transitionOperation(t, f.s, next, "cancelled", "cancelled")
	o = transitionOperation(t, f.s, confirmed, "rolling_back", "rolling_back")
	o = transitionOperation(t, f.s, o, "rollback_failed", "rollback_failed")
	if _, _, err := f.s.CreateConfigOperation(context.Background(), operationRequest(f, node.ID, 3)); !errors.Is(err, ErrConflict) {
		t.Fatal("rollback failure did not reserve node", err)
	}
	o = transitionOperation(t, f.s, o, "rolling_back", "rolling_back")
	transitionOperation(t, f.s, o, "rolled_back", "rolled_back")
	createOperation(t, f.s, operationRequest(f, node.ID, 4))
}

func TestConfigOperationRestartNodeDeletionAndDeadline(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	input := operationRequest(f, node.ID, 1)
	o := createOperation(t, f.s, input)
	for _, step := range [][2]string{{"validated", "validated"}, {"prepared", "prepared"}, {"applying", "applying"}, {"outcome_unknown", "outcome_unknown"}} {
		o = transitionOperation(t, f.s, o, step[0], step[1])
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = Open(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := f.s.GetConfigOperation(context.Background(), o.OperationID)
	if err != nil || !reflect.DeepEqual(recovered, o) {
		t.Fatal("restart lost uncertain operation", recovered, err)
	}
	if err := f.s.DeleteNode(context.Background(), node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetConfigOperation(context.Background(), o.OperationID); err != nil {
		t.Fatal("node deletion removed audit owner", err)
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), o.OperationID)
	if err != nil || len(events) != 5 {
		t.Fatal("node deletion cascaded audit", events, err)
	}
	operations, err := f.s.ListConfigOperations(context.Background(), node.ID, 10)
	if err != nil || len(operations) != 1 {
		t.Fatal("historical node operation no longer queryable", operations, err)
	}
	if _, _, err := f.s.CreateConfigOperation(context.Background(), operationRequest(f, node.ID, 2)); !errors.Is(err, ErrNotFound) {
		t.Fatal("operation created for missing node", err)
	}
	f.clock.Store(input.DeadlineAtMS + 1)
	if replay, ok, err := f.s.CreateConfigOperation(context.Background(), input); err != nil || !ok || replay.State != "outcome_unknown" {
		t.Fatal("expired idempotent query did not recover original state", replay, ok, err)
	}
	o = transitionOperation(t, f.s, recovered, "rolling_back", "rolling_back")
	transitionOperation(t, f.s, o, "rolled_back", "rolled_back")

	node = f.create(DefaultNodeConfig("next"))
	input = operationRequest(f, node.ID, 3)
	o = createOperation(t, f.s, input)
	f.clock.Store(input.DeadlineAtMS)
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: 1, NextState: "validated", Code: "validated", CandidateDigest: strings.Repeat("b", 64)}); !errors.Is(err, ErrConflict) {
		t.Fatal("expired operation advanced toward apply", err)
	}
	transitionOperation(t, f.s, o, "cancelled", "cancelled")
}

func TestConfigOperationAuditRejectsValuesAndUnknownCodes(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("node"))
	input := operationRequest(f, node.ID, 1)
	input.Changes[0].Fields = []string{"httpPassword=synthetic-secret"}
	if _, _, err := f.s.CreateConfigOperation(context.Background(), input); !errors.Is(err, ErrInvalid) {
		t.Fatal("audit admitted a configuration value", err)
	}
	input.Changes[0].Fields = []string{"httpPassword", "secretKey"}
	o := createOperation(t, f.s, input)
	if _, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: 1, NextState: "rejected", Code: "error includes synthetic-secret"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("audit admitted an arbitrary error", err)
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), o.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(events)
	if strings.Contains(string(data), "synthetic-secret") || !strings.Contains(string(data), "httpPassword") {
		t.Fatal("audit did not preserve names-only summary", string(data))
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err := f.s.ListConfigOperations(context.Background(), node.ID, limit); !errors.Is(err, ErrInvalid) {
			t.Fatal("unbounded query accepted", err)
		}
	}
}

func versionSixDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "control.sqlite")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	oldSchema, _, found := strings.Cut(Schema, operationSchemaMarker)
	if !found {
		t.Fatal("v6 schema boundary missing")
	}
	if _, err := db.Exec(oldSchema + "PRAGMA application_id=1179798836; PRAGMA user_version=6; PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestConfigOperationMigrationBacksUpCommittedWALBeforeV7(t *testing.T) {
	path, db := versionSixDatabase(t)
	if _, err := db.Exec("INSERT INTO nodes(id,name,token_sha256,created_at_ms,updated_at_ms) VALUES(7,'wal-node',?,1,2); INSERT INTO node_groups(id,name) VALUES(3,'retained-group'); INSERT INTO node_group_members(group_id,node_id) VALUES(3,7);", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatal("test did not retain committed WAL data", err)
	}
	s, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	backup := s.MigrationBackupPath()
	if backup == "" {
		t.Fatal("migration lacks a recovery database")
	}
	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("migration backup is not private", err)
	}
	saved, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	var version, groups, operations int
	var name, integrity string
	if err := saved.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal("backup lost pre-migration schema", version, err)
	}
	if err := saved.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("backup is not a consistent SQLite database", integrity, err)
	}
	if err := saved.QueryRow("SELECT name FROM nodes WHERE id=7").Scan(&name); err != nil || name != "wal-node" {
		t.Fatal("backup omitted committed WAL node", name, err)
	}
	if err := saved.QueryRow("SELECT count(*) FROM node_group_members WHERE node_id=7 AND group_id=3").Scan(&groups); err != nil || groups != 1 {
		t.Fatal("backup lost existing membership", groups, err)
	}
	if err := saved.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='config_operations'").Scan(&operations); err != nil || operations != 0 {
		t.Fatal("backup was created after migration", operations, err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 12 {
		t.Fatal("v7 migration did not complete", version, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.MigrationBackupPath() != "" {
		t.Fatal("unchanged v7 database was migrated again")
	}
}

func TestConfigOperationMigrationBackupFailurePreventsDDL(t *testing.T) {
	path, db := versionSixDatabase(t)
	called := false
	s, err := openWithMigrationBackup(Config{Path: path}, func(context.Context, *sql.DB, string, int) (string, error) {
		called = true
		return "", errors.New("synthetic consistent backup failure")
	})
	if err == nil || !called {
		if s != nil {
			s.Close()
		}
		t.Fatal("migration continued after backup failure", err)
	}
	var version, operations int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal("failed backup changed schema version", version, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='config_operations'").Scan(&operations); err != nil || operations != 0 {
		t.Fatal("failed backup created partial tables", operations, err)
	}
}

func TestConfigOperationV8FactsActorAndActiveKeyset(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	first := f.create(DefaultNodeConfig("first"))
	second := f.create(DefaultNodeConfig("second"))
	o := createOperation(t, f.s, operationRequest(f, first.ID, 1))
	old := o
	view := &shared.ConfigOperationView{OperationID: o.OperationID, BaseRevision: o.BaseRevision, ContextRevision: strings.Repeat("c", 64), OldDigest: strings.Repeat("d", 64), CandidateDigest: strings.Repeat("b", 64), State: "prepared", CreatedAtMS: o.CreatedAtMS, UpdatedAtMS: o.CreatedAtMS, DeadlineAtMS: o.DeadlineAtMS}
	var err error
	o, err = f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: o.Version, NextState: "validated", Code: "validated", Actor: "reviewer", CandidateDigest: view.CandidateDigest, Agent: view, AgentReceivedAtMS: f.clock.Load()})
	if err != nil {
		t.Fatal(err)
	}
	view.ContextRevision = strings.Repeat("e", 64)
	if o.Agent.ContextRevision == view.ContextRevision {
		t.Fatal("caller retained observation ownership")
	}
	if _, err = f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: old.Version, NextState: "failed", Code: "failed", Actor: "intruder"}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale CAS", err)
	}
	o = transitionOperation(t, f.s, o, "prepared", "prepared")
	o = transitionOperation(t, f.s, o, "applying", "applying")
	confirmed := *o.Agent
	confirmed.State = "confirmed"
	confirmed.StorePersisted = true
	confirmed.RuntimeApplied = true
	confirmed.RuntimeLoaded = true
	confirmed.ResourcesReady = true
	confirmed.BusinessChecked = true
	o, err = f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: o.Version, NextState: "verifying", Code: "verifying", Actor: "applier", Agent: &confirmed, AgentReceivedAtMS: f.clock.Load()})
	if err != nil {
		t.Fatal(err)
	}
	o = transitionOperation(t, f.s, o, "confirmed", "confirmed")
	other := createOperation(t, f.s, operationRequest(f, second.ID, 2))
	active, err := f.s.ListActiveConfigOperations(context.Background(), "", 1)
	if err != nil || len(active) != 1 || active[0].OperationID != other.OperationID {
		t.Fatal("active scan", active, err)
	}
	active, err = f.s.ListActiveConfigOperations(context.Background(), other.OperationID, 1)
	if err != nil || len(active) != 0 {
		t.Fatal("keyset repeated", active, err)
	}
	path := f.s.cfg.Path
	cfg := f.s.cfg
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.GetConfigOperation(context.Background(), o.OperationID)
	if err != nil || saved.Agent == nil || !saved.Agent.StorePersisted || !saved.Agent.RuntimeApplied || !saved.Agent.RuntimeLoaded || !saved.Agent.ResourcesReady || !saved.Agent.BusinessChecked {
		t.Fatal("facts lost after restart", saved, err)
	}
	events, err := reopened.ListConfigOperationEvents(context.Background(), o.OperationID)
	if err != nil || events[1].Actor != "reviewer" || events[4].Actor != "applier" {
		t.Fatal("actor not atomic", events, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "intruder") {
		t.Fatal("failed CAS appended audit")
	}
}

func TestConfigOperationV7MigrationBacksUpAuditAndAddsActors(t *testing.T) {
	path, db := versionSixDatabase(t)
	operations, err := schemaSection(operationSchemaMarker, observationSchemaMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(operations + "PRAGMA user_version=7;"); err != nil {
		t.Fatal(err)
	}
	id := configOperationID(1)
	_, err = db.Exec("INSERT INTO config_operations(operation_id,node_id,service_id,base_revision,candidate_digest,creator,deadline_at_ms,idempotency_key,request_digest,state,version,created_at_ms,updated_at_ms) VALUES(?,1,'primary',?,'','original-actor',900000,?,?,'draft',1,1,1)", id, strings.Repeat("a", 64), configOperationID(2), strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO config_operation_events(operation_id,version,state,code,changes_json,created_at_ms) VALUES(?,1,'draft','created','[]',1)", id); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	events, err := migrated.ListConfigOperationEvents(context.Background(), id)
	if err != nil || len(events) != 1 || events[0].Actor != "original-actor" {
		t.Fatal("v7 audit not preserved/backfilled", events, err)
	}
	saved, err := sql.Open("sqlite", migrated.MigrationBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	var version, count int
	if err = saved.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 7 {
		t.Fatal("backup not v7", version, err)
	}
	if err = saved.QueryRow("SELECT count(*) FROM config_operation_events").Scan(&count); err != nil || count != 1 {
		t.Fatal("backup omitted WAL audit", count, err)
	}
}

func TestConfigOperationCloneSummaryPreservesSourceAndRejectsInvalidSource(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("clone"))
	input := operationRequest(f, node.ID, 1)
	input.Changes = []ConfigOperationChange{{Kind: "proxy", Name: "new-object", Action: "create", CloneFrom: "original-object", Fields: []string{}}}
	operation := createOperation(t, f.s, input)
	events, err := f.s.ListConfigOperationEvents(context.Background(), operation.OperationID)
	if err != nil || len(events) != 1 || events[0].Changes[0].CloneFrom != "original-object" {
		t.Fatal("clone source missing from audit", events, err)
	}
	for _, change := range []ConfigOperationChange{{Kind: "proxy", Name: "new", Action: "update", CloneFrom: "old"}, {Kind: "proxy", Name: "same", Action: "create", CloneFrom: "same"}, {Kind: "proxy", Name: "new", Action: "create", CloneFrom: "bad\nsource"}, {Kind: "proxy", Name: "new", Action: "create", CloneFrom: strings.Repeat("x", 257)}} {
		if _, err = operationChanges([]ConfigOperationChange{change}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid clone audit accepted", err)
		}
	}
}
