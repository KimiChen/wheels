package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func restoreRequest(node *Node, n int) ClaimConfigRestoreRequest {
	return ClaimConfigRestoreRequest{ID: configOperationID(n), NodeID: node.ID, ServiceID: configOperationID(101), Epoch: configOperationID(102), BackupServiceID: configOperationID(103), ReplacedServiceID: configOperationID(104), ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), Creator: "restoring-admin", ExpectedTokenSHA256: node.TokenSHA256}
}

func restoreOldOperation(t *testing.T, f *fixture, node *Node, service string) *ConfigOperation {
	t.Helper()
	request := operationRequest(f, node.ID, 1001)
	request.ServiceID = service
	o := createOperation(t, f.s, request)
	o = transitionOperation(t, f.s, o, "validated", "validated")
	view := &shared.ConfigOperationView{OperationID: o.OperationID, BaseRevision: o.BaseRevision, ContextRevision: strings.Repeat("c", 64), OldDigest: strings.Repeat("d", 64), CandidateDigest: strings.Repeat("b", 64), State: "prepared", CreatedAtMS: o.CreatedAtMS, UpdatedAtMS: o.CreatedAtMS, DeadlineAtMS: o.DeadlineAtMS}
	o, err := f.s.TransitionConfigOperation(context.Background(), o.OperationID, ConfigOperationTransition{ExpectedVersion: o.Version, NextState: "outcome_unknown", Code: "connection_lost", Agent: view, AgentReceivedAtMS: o.CreatedAtMS})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestConfigRestoreClaimSupersedesExactLineageAndReplaysAcrossAdministrators(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("restored"))
	request := restoreRequest(node, 1)
	old := restoreOldOperation(t, f, node, request.BackupServiceID)
	request.ExpectedActiveOperationID, request.ExpectedVersion = old.OperationID, old.Version
	receipt, replayed, err := f.s.ClaimConfigRestore(context.Background(), request)
	if err != nil || replayed || receipt.State != "pending" || receipt.Version != 1 {
		t.Fatal("claim did not create a pending receipt", receipt, replayed, err)
	}
	after, err := f.s.GetConfigOperation(context.Background(), old.OperationID)
	if err != nil || after.State != "cancelled" || after.Version != old.Version+1 || !reflect.DeepEqual(after.Agent, old.Agent) || after.AgentReceivedAtMS != old.AgentReceivedAtMS {
		t.Fatal("supersession lost prior Agent facts", after, err)
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), old.OperationID)
	if err != nil || events[len(events)-1].Code != "restore_superseded" || events[len(events)-1].Actor != request.Creator {
		t.Fatal("supersession lacks explicit administrator audit", events, err)
	}
	request.ID, request.Creator = configOperationID(2), "another-admin"
	again, replayed, err := f.s.ClaimConfigRestore(context.Background(), request)
	if err != nil || !replayed || !reflect.DeepEqual(again, receipt) {
		t.Fatal("retry did not retain the original receipt and actor", again, replayed, err)
	}
	currentEvents, _ := f.s.ListConfigOperationEvents(context.Background(), old.OperationID)
	if len(currentEvents) != len(events) {
		t.Fatal("retry repeated the supersession audit")
	}
	request.ManifestDigest = strings.Repeat("d", 64)
	if _, _, err := f.s.ClaimConfigRestore(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatal("same epoch accepted changed content", err)
	}
	data, err := json.Marshal(receipt)
	if err != nil || strings.Contains(string(data), "token_sha256") || strings.Contains(string(data), node.TokenSHA256) {
		t.Fatal("private token hash escaped receipt JSON", err)
	}
}

func TestConfigRestoreClaimCASLineageAndAuditFailureAreAtomic(t *testing.T) {
	for _, failure := range []string{"unexpected-active", "stale-version", "unrelated-service", "audit-failure", "insert-failure"} {
		t.Run(failure, func(t *testing.T) {
			f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
			node := f.create(DefaultNodeConfig("restored"))
			request := restoreRequest(node, 1)
			service := request.ReplacedServiceID
			if failure == "unrelated-service" {
				service = configOperationID(999)
			}
			old := restoreOldOperation(t, f, node, service)
			request.ExpectedActiveOperationID, request.ExpectedVersion = old.OperationID, old.Version
			switch failure {
			case "unexpected-active":
				request.ExpectedActiveOperationID, request.ExpectedVersion = "", 0
			case "stale-version":
				request.ExpectedVersion--
			case "audit-failure":
				if _, err := f.s.db.Exec("CREATE TRIGGER reject_restore_audit BEFORE INSERT ON config_operation_events WHEN NEW.code='restore_superseded' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END"); err != nil {
					t.Fatal(err)
				}
			case "insert-failure":
				if _, err := f.s.db.Exec("CREATE TRIGGER reject_restore BEFORE INSERT ON config_restores BEGIN SELECT RAISE(ABORT,'synthetic failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := f.s.ClaimConfigRestore(context.Background(), request); err == nil {
				t.Fatal("unsafe claim accepted")
			}
			after, err := f.s.GetConfigOperation(context.Background(), old.OperationID)
			if err != nil || !reflect.DeepEqual(after, old) {
				t.Fatal("failed claim modified old operation", after, err)
			}
			if _, err := f.s.GetConfigRestore(context.Background(), node.ID, request.Epoch); !errors.Is(err, ErrNotFound) {
				t.Fatal("failed claim persisted a partial receipt", err)
			}
		})
	}
}

func TestConfigRestoreConcurrentClaimHasOneReceiptAndOneSupersession(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("restored"))
	request := restoreRequest(node, 1)
	old := restoreOldOperation(t, f, node, request.BackupServiceID)
	request.ExpectedActiveOperationID, request.ExpectedVersion = old.OperationID, old.Version
	var group sync.WaitGroup
	start := make(chan struct{})
	errorsFound := make(chan error, 8)
	results := make(chan *ConfigRestore, 8)
	for n := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			input := request
			input.ID = configOperationID(n + 1)
			<-start
			result, _, err := f.s.ClaimConfigRestore(context.Background(), input)
			errorsFound <- err
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(errorsFound)
	close(results)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *ConfigRestore
	for result := range results {
		if first == nil {
			first = result
		} else if !reflect.DeepEqual(first, result) {
			t.Fatal("concurrent claims returned different receipts")
		}
	}
	events, _ := f.s.ListConfigOperationEvents(context.Background(), old.OperationID)
	count := 0
	for _, event := range events {
		if event.Code == "restore_superseded" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("supersession was not unique", count)
	}
}

func TestConfigRestoreConfirmationCASRotationDeletionAndRetainedHistory(t *testing.T) {
	for _, action := range []string{"confirm", "rotation-before-claim", "rotation-after-claim", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
			node := f.create(DefaultNodeConfig("restored"))
			request := restoreRequest(node, 1)
			if action == "rotation-before-claim" {
				if err := f.s.RotateToken(context.Background(), node.ID, strings.Repeat("f", 64)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := f.s.ClaimConfigRestore(context.Background(), request); !errors.Is(err, ErrConflict) {
					t.Fatal("claim crossed credential rotation", err)
				}
				return
			}
			receipt, _, err := f.s.ClaimConfigRestore(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ConfirmConfigRestore(context.Background(), receipt.ID, 9); !errors.Is(err, ErrConflict) {
				t.Fatal("confirmation ignored stale CAS", err)
			}
			switch action {
			case "rotation-after-claim":
				if err := f.s.RotateToken(context.Background(), node.ID, strings.Repeat("f", 64)); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := f.s.DeleteNode(context.Background(), node.ID); err != nil {
					t.Fatal(err)
				}
			}
			confirmed, err := f.s.ConfirmConfigRestore(context.Background(), receipt.ID, receipt.Version)
			if action == "confirm" {
				if err != nil || confirmed.State != "acknowledged" || confirmed.Version != 2 {
					t.Fatal("receipt not acknowledged", confirmed, err)
				}
				again, err := f.s.ConfirmConfigRestore(context.Background(), receipt.ID, receipt.Version)
				if err != nil || !reflect.DeepEqual(again, confirmed) {
					t.Fatal("lost confirmation reply is not retryable", again, err)
				}
			} else if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
				t.Fatal("confirmation crossed revoked identity", confirmed, err)
			}
			if _, err := f.s.GetConfigRestore(context.Background(), node.ID, request.Epoch); err != nil {
				t.Fatal("revocation deleted restore audit", err)
			}
		})
	}
}

func versionEightDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, db := versionSixDatabase(t)
	operations, err := schemaSection(operationSchemaMarker, restoreSchemaMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(operations + "PRAGMA user_version=8;"); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestConfigRestoreSchemaNineMigrationPreservesWALAndPrivateRecoveryImage(t *testing.T) {
	path, db := versionEightDatabase(t)
	if _, err := db.Exec("INSERT INTO nodes(id,name,token_sha256,created_at_ms,updated_at_ms) VALUES(7,'retained',?,1,2)", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	store, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version, count int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatal("schema not upgraded", version, err)
	}
	if err := store.db.QueryRow("SELECT count(*) FROM nodes WHERE id=7").Scan(&count); err != nil || count != 1 {
		t.Fatal("node disappeared during upgrade", count, err)
	}
	saved, err := sql.Open("sqlite", store.MigrationBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	if err := saved.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
		t.Fatal("recovery image was not taken before DDL", version, err)
	}
	if err := saved.QueryRow("SELECT count(*) FROM nodes WHERE id=7").Scan(&count); err != nil || count != 1 {
		t.Fatal("recovery image omitted committed WAL", count, err)
	}
	if err := saved.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='config_restores'").Scan(&count); err != nil || count != 0 {
		t.Fatal("recovery image contains new schema", count, err)
	}
}

func TestConfigRestoreSchemaNineBackupAndDDLFailureLeaveVersionEight(t *testing.T) {
	for _, failure := range []string{"backup", "ddl"} {
		t.Run(failure, func(t *testing.T) {
			path, db := versionEightDatabase(t)
			if failure == "ddl" {
				if _, err := db.Exec("CREATE TABLE config_restores(existing TEXT)"); err != nil {
					t.Fatal(err)
				}
			}
			backup := backupBeforeMigration
			if failure == "backup" {
				backup = func(context.Context, *sql.DB, string, int) (string, error) {
					return "", errors.New("synthetic backup failure")
				}
			}
			if store, err := openWithMigrationBackup(Config{Path: path}, backup); err == nil {
				store.Close()
				t.Fatal("failed migration reported success")
			}
			var version int
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
				t.Fatal("failed migration changed schema version", version, err)
			}
		})
	}
}

func TestConfigRestoreActiveLookupMatchesProtocolLeaseStates(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("restored"))
	if _, err := f.s.GetActiveConfigOperation(context.Background(), node.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("empty node has an active operation", err)
	}
	o := createOperation(t, f.s, operationRequest(f, node.ID, 1))
	for _, state := range []string{"draft", "validated", "prepared", "applying", "verifying", "confirmed", "rejected", "conflict", "failed", "outcome_unknown", "cancelled", "rolling_back", "rolled_back", "rollback_failed"} {
		if _, err := f.s.db.Exec("UPDATE config_operations SET state=? WHERE operation_id=?", state, o.OperationID); err != nil {
			t.Fatal(err)
		}
		active, err := f.s.GetActiveConfigOperation(context.Background(), node.ID)
		if shared.ConfigOperationActive(state) {
			if err != nil || active.OperationID != o.OperationID || active.State != state {
				t.Fatal("active lookup omitted lease state", state, err)
			}
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatal("terminal operation retained a lease", state, err)
		}
	}
}
