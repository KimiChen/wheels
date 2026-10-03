package control

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

func serverRestoreEvidence(n int, complete bool) ServerRestoreReceipt {
	r := ConfigRestore{ID: configOperationID(n), NodeID: "1", ServiceID: configOperationID(n + 10000), Epoch: configOperationID(n + 20000), BackupServiceID: configOperationID(n + 30000), ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), State: "pending", Creator: "test-admin", CreatedAtMS: int64(n + 1), UpdatedAtMS: int64(n + 1), Version: 1, TokenSHA256: strings.Repeat("d", 64)}
	e := ServerRestoreReceipt{Receipt: r, CurrentTokenSHA256: r.TokenSHA256}
	if complete {
		e.Receipt.State = "acknowledged"
		at := e.Receipt.UpdatedAtMS
		e.CompletedAtMS = &at
	}
	return e
}

func serverRestoreChain(edges int) []ServerRestoreReceipt {
	items := make([]ServerRestoreReceipt, edges+1)
	for i := range items {
		items[i] = serverRestoreEvidence(i+1, i == edges)
		if i > 0 {
			items[i].Receipt.ReplacedServiceID = items[i-1].Receipt.ServiceID
		}
	}
	return items
}

func TestServerRestoreConfirmationOperationStates(t *testing.T) {
	for _, state := range []string{"draft", "validated", "prepared", "applying", "verifying", "outcome_unknown", "rolling_back", "rollback_failed"} {
		t.Run(state, func(t *testing.T) {
			got := EvaluateServerRestoreConfirmation(map[string]int64{state: 1}, nil)
			if got.Ready || got.Code != "active_operations" || got.ActiveOperations != 1 {
				t.Fatal(got)
			}
		})
	}
	states := map[string]int64{}
	for _, state := range []string{"confirmed", "rejected", "conflict", "failed", "cancelled", "rolled_back"} {
		states[state] = 2
	}
	if got := EvaluateServerRestoreConfirmation(states, nil); !got.Ready || got.Code != "ok" {
		t.Fatal(got)
	}
	for _, states := range []map[string]int64{{"future": 1}, {"confirmed": -1}} {
		if got := EvaluateServerRestoreConfirmation(states, nil); got.Ready || got.Code != "invalid_operation_state" {
			t.Fatal(got)
		}
	}
}

func TestServerRestoreConfirmationCompletedHistorySurvivesRevocation(t *testing.T) {
	for _, token := range []string{"", strings.Repeat("f", 64)} {
		e := serverRestoreEvidence(1, true)
		e.CurrentTokenSHA256 = token
		got := EvaluateServerRestoreConfirmation(nil, []ServerRestoreReceipt{e})
		if !got.Ready || got.Completed != 1 || got.Superseded != 0 {
			t.Fatal(got)
		}
	}
}

func TestServerRestoreConfirmationReplacementEvidence(t *testing.T) {
	cases := []struct {
		name, code string
		mutate     func([]ServerRestoreReceipt) []ServerRestoreReceipt
	}{
		{"completed-chain", "ok", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { return v }},
		{"superseded-lost-ack", "ok", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			at := v[0].Receipt.CreatedAtMS
			v[0].CompletedAtMS = &at
			return v
		}},
		{"missing-successor", "receipt_unresolved", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { return v[:1] }},
		{"backup-is-not-replacement", "receipt_unresolved", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[1].Receipt.BackupServiceID = v[0].Receipt.ServiceID
			v[1].Receipt.ReplacedServiceID = ""
			return v
		}},
		{"wrong-node", "receipt_unresolved", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[1].Receipt.NodeID = "2"
			return v
		}},
		{"deleted-node", "receipt_binding", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { v[0].CurrentTokenSHA256 = ""; return v }},
		{"rotated-current-token", "receipt_binding", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[0].CurrentTokenSHA256 = strings.Repeat("e", 64)
			return v
		}},
		{"replacement-token-changed", "receipt_binding", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[1].Receipt.TokenSHA256 = strings.Repeat("e", 64)
			return v
		}},
		{"completed-endpoint-binding", "receipt_binding", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { v[2].CurrentTokenSHA256 = ""; return v }},
		{"fork", "receipt_chain_ambiguous", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			e := serverRestoreEvidence(5, true)
			e.Receipt.ReplacedServiceID = v[0].Receipt.ServiceID
			return append(v, e)
		}},
		{"time-reversed", "receipt_chain_order", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { v[1].Receipt.CreatedAtMS = 1; return v }},
		{"cycle", "receipt_chain_cycle", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v = v[:2]
			for i := range v {
				v[i].Receipt.CreatedAtMS = 1
				v[i].Receipt.UpdatedAtMS = 1
			}
			v[0].Receipt.ReplacedServiceID = v[1].Receipt.ServiceID
			return v
		}},
		{"pending-with-completion", "receipt_acknowledgement_required", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { v[2].Receipt.State = "pending"; return v }},
		{"zero-completion", "invalid_completion", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			at := int64(0)
			v[2].CompletedAtMS = &at
			return v
		}},
		{"completion-before-creation", "invalid_completion", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[2].Receipt.CreatedAtMS++
			v[2].Receipt.UpdatedAtMS++
			return v
		}},
		{"invalid-identity", "invalid_receipt", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[2].Receipt.Epoch = "private-invalid"
			return v
		}},
		{"duplicate-service", "invalid_receipt", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[2].Receipt.ServiceID = v[0].Receipt.ServiceID
			return v
		}},
		{"duplicate-id", "invalid_receipt", func(v []ServerRestoreReceipt) []ServerRestoreReceipt { v[2].Receipt.ID = v[0].Receipt.ID; return v }},
		{"duplicate-epoch", "invalid_receipt", func(v []ServerRestoreReceipt) []ServerRestoreReceipt {
			v[2].Receipt.Epoch = v[0].Receipt.Epoch
			return v
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateServerRestoreConfirmation(nil, test.mutate(serverRestoreChain(2)))
			if got.Code != test.code || got.Ready != (test.code == "ok") {
				t.Fatal(got)
			}
			if got.Ready && (got.Completed != 1 || got.Superseded != 2) {
				t.Fatal(got)
			}
		})
	}
	for _, edges := range []int{MaxServerRestoreReplacementDepth, MaxServerRestoreReplacementDepth + 1} {
		got := EvaluateServerRestoreConfirmation(nil, serverRestoreChain(edges))
		if got.Ready != (edges == MaxServerRestoreReplacementDepth) {
			t.Fatal(edges, got)
		}
		if !got.Ready && got.Code != "receipt_chain_limit" {
			t.Fatal(got)
		}
	}
	items := make([]ServerRestoreReceipt, MaxServerRestoreReceipts)
	for i := range items {
		items[i] = serverRestoreEvidence(i+1, true)
	}
	if got := EvaluateServerRestoreConfirmation(nil, items); !got.Ready {
		t.Fatal(got)
	}
	if got := EvaluateServerRestoreConfirmation(nil, append(items, serverRestoreEvidence(MaxServerRestoreReceipts+1, true))); got.Ready || got.Code != "receipt_limit" {
		t.Fatal(got)
	}
}

func TestServerRestoreConfirmationReadOnlySQLiteSnapshot(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("restore-confirm-readonly"))
	ctx := context.Background()
	items := serverRestoreChain(2)
	for i := range items {
		items[i].Receipt.NodeID = node.ID
		items[i].Receipt.TokenSHA256 = node.TokenSHA256
		items[i].CurrentTokenSHA256 = node.TokenSHA256
	}
	if err := f.s.call(ctx, func(tx *sql.Tx) error {
		for _, e := range items {
			r := e.Receipt
			_, err := tx.Exec("INSERT INTO config_restores("+restoreColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", r.ID, r.NodeID, r.ServiceID, r.Epoch, r.BackupServiceID, r.ReplacedServiceID, r.ManifestDigest, r.ContextRevision, r.StoreDigest, r.State, r.Creator, r.CreatedAtMS, r.UpdatedAtMS, r.Version, r.TokenSHA256)
			if err != nil {
				return err
			}
			if e.CompletedAtMS != nil {
				if _, err = tx.Exec("INSERT INTO config_restore_completions(receipt_id,confirmed_at_ms) VALUES(?,?)", r.ID, *e.CompletedAtMS); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := EvaluateServerRestoreConfirmation(nil, items)
	for i := 0; i < 2; i++ {
		got, err := f.s.CheckServerRestoreConfirmation(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal(got, want, err)
		}
	}
	if err := f.s.readAuditSnapshot(ctx, func(tx *sql.Tx) error {
		var queryOnly int
		if err := tx.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
			return err
		}
		if queryOnly != 1 {
			t.Fatal("snapshot not query-only")
		}
		if _, err := tx.Exec("UPDATE config_restores SET state='acknowledged'"); err == nil {
			t.Fatal("read snapshot permits writing")
		}
		for _, e := range items {
			r, err := scanConfigRestore(tx.QueryRow("SELECT "+restoreColumns+" FROM config_restores WHERE id=?", e.Receipt.ID))
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(*r, e.Receipt) {
				t.Fatal("readonly predicate changed receipt")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.s.CheckServerRestoreConfirmation(cancelled); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err := f.s.RotateToken(ctx, node.ID, strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.CheckServerRestoreConfirmation(ctx); err != nil || got.Ready || got.Code != "receipt_binding" {
		t.Fatal(got, err)
	}
}

func TestServerRestoreConfirmationRejectsMalformedSQLiteEvidence(t *testing.T) {
	for _, kind := range []string{"orphan-completion", "pending-completion", "unknown-operation", "missing-schema"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
			ctx := context.Background()
			node := f.create(DefaultNodeConfig("corrupt-confirm"))
			var sqlText string
			switch kind {
			case "orphan-completion":
				sqlText = "INSERT INTO config_restore_completions(receipt_id,confirmed_at_ms) VALUES('orphan',1)"
			case "pending-completion":
				r, _, err := f.s.ClaimConfigRestore(ctx, restoreRequest(node, 1))
				if err != nil {
					t.Fatal(err)
				}
				sqlText = "INSERT INTO config_restore_completions(receipt_id,confirmed_at_ms) VALUES('" + r.ID + "'," + "9999999999999)"
			case "unknown-operation":
				o := createOperation(t, f.s, operationRequest(f, node.ID, 9))
				sqlText = "UPDATE config_operations SET state='future' WHERE operation_id='" + o.OperationID + "'"
			case "missing-schema":
				sqlText = "DROP TABLE config_restore_completions"
			}
			// Deliberately model a database modified by an external writer. The
			// normal Store correctly prevents these states from being generated.
			db, err := sql.Open("sqlite", f.s.cfg.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec("PRAGMA ignore_check_constraints=ON;" + sqlText); err != nil {
				t.Fatal(err)
			}
			got, err := f.s.CheckServerRestoreConfirmation(ctx)
			if kind == "missing-schema" {
				if err == nil || got.Ready {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || got.Ready {
				t.Fatal(got, err)
			}
			want := "invalid_completion"
			if kind == "pending-completion" {
				want = "receipt_acknowledgement_required"
			}
			if kind == "unknown-operation" {
				want = "invalid_operation_state"
			}
			if got.Code != want {
				t.Fatal(got)
			}
		})
	}
}

func TestServerRestoreConfirmationDelayedAcknowledgementKeepsObservedFact(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("delayed-ack"))
	ctx := context.Background()
	r, _, err := f.s.ClaimConfigRestore(ctx, restoreRequest(node, 1))
	if err != nil {
		t.Fatal(err)
	}
	// Existing recovery semantics preserve a real Agent-confirmed fact even
	// when the Admin acknowledgement reply was lost. This alone is not Ready.
	if err = f.s.ObserveConfigRestoreConfirmed(ctx, node.ID, r.ServiceID, node.TokenSHA256, completedRestoreInfo(r)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.CheckServerRestoreConfirmation(ctx); err != nil || got.Ready || got.Code != "receipt_acknowledgement_required" {
		t.Fatal(got, err)
	}
	var at int64
	if err = f.s.db.QueryRow("SELECT confirmed_at_ms FROM config_restore_completions WHERE receipt_id=?", r.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(1000)
	r, err = f.s.ConfirmConfigRestore(ctx, r.ID, r.Version)
	if err != nil {
		t.Fatal(err)
	}
	if r.UpdatedAtMS <= at {
		t.Fatal("fixture did not delay acknowledgement")
	}
	if got, err := f.s.CheckServerRestoreConfirmation(ctx); err != nil || !got.Ready || got.Completed != 1 {
		t.Fatal(got, err)
	}
	var after int64
	if err = f.s.db.QueryRow("SELECT confirmed_at_ms FROM config_restore_completions WHERE receipt_id=?", r.ID).Scan(&after); err != nil || after != at {
		t.Fatal("ack rewrote Agent observation", err)
	}
}
