package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func completedRestoreInfo(r *ConfigRestore) shared.RestoreInfo {
	return shared.RestoreInfo{State: "confirmed", Epoch: r.Epoch, BackupServiceID: r.BackupServiceID, ReplacedServiceID: r.ReplacedServiceID, ManifestDigest: r.ManifestDigest, ContextRevision: r.ContextRevision, StoreDigest: r.StoreDigest, AcknowledgementID: r.ID, RuntimeLoaded: true, ResourcesReady: true}
}

func TestRestoreCompletionProtectsLineageUntilProvenCompletionTTL(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("restoring"))
	req := restoreRequest(node, 100)
	old := restoreOldOperation(t, f, node, req.BackupServiceID)
	req.ExpectedActiveOperationID, req.ExpectedVersion = old.OperationID, old.Version
	receipt, _, err := f.s.ClaimConfigRestore(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = f.s.ConfirmConfigRestore(context.Background(), receipt.ID, receipt.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(91 * 24 * time.Hour / time.Millisecond))
	if r, err := f.s.PruneAudit(context.Background()); err != nil || r.Rows+r.Events != 0 {
		t.Fatal("ack released unfinished restoration", r, err)
	}
	info := completedRestoreInfo(receipt)
	if err := f.s.ObserveConfigRestoreConfirmed(context.Background(), node.ID, receipt.ServiceID, node.TokenSHA256, info); err != nil {
		t.Fatal(err)
	}
	var initial int64
	if err := f.s.db.QueryRow("SELECT confirmed_at_ms FROM config_restore_completions WHERE receipt_id=?", receipt.ID).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(90 * 24 * time.Hour / time.Millisecond))
	if err := f.s.ObserveConfigRestoreConfirmed(context.Background(), node.ID, receipt.ServiceID, node.TokenSHA256, info); err != nil {
		t.Fatal(err)
	}
	var repeated int64
	_ = f.s.db.QueryRow("SELECT confirmed_at_ms FROM config_restore_completions WHERE receipt_id=?", receipt.ID).Scan(&repeated)
	if repeated != initial {
		t.Fatal("inspection extended retention clock")
	}
	if r, err := f.s.PruneAudit(context.Background()); err != nil || r.Rows+r.Events != 0 {
		t.Fatal("TTL equality removed protected history", r, err)
	}
	f.clock.Add(1)
	if r, err := f.s.PruneAudit(context.Background()); err != nil || r.Rows == 0 || r.Events == 0 {
		t.Fatal("proven completion did not release expired history", r, err)
	}
	events, err := f.s.ListConfigOperationEvents(context.Background(), old.OperationID)
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
	current, err := f.s.GetConfigRestore(context.Background(), node.ID, receipt.Epoch)
	if err != nil || current.State != "acknowledged" {
		t.Fatal("observation rewrote consent state", current, err)
	}
	if err := f.s.db.QueryRow("SELECT confirmed_at_ms FROM config_restore_completions WHERE receipt_id=?", receipt.ID).Scan(&repeated); err != nil || repeated != initial {
		t.Fatal("proof was garbage collected", err)
	}
}

func TestRestoreCompletionRejectsMismatchesAndRollsBackAuditFailure(t *testing.T) {
	for _, which := range []string{"unverified", "acknowledged", "service", "ack", "backup", "replaced", "manifest", "context", "store", "token", "rotated", "audit"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
			node := f.create(DefaultNodeConfig("restoring"))
			req := restoreRequest(node, 100)
			receipt, _, err := f.s.ClaimConfigRestore(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			info := completedRestoreInfo(receipt)
			service, token := receipt.ServiceID, node.TokenSHA256
			switch which {
			case "unverified":
				info.RuntimeLoaded = false
			case "acknowledged":
				info.State = "acknowledged"
			case "service":
				service = configOperationID(999)
			case "ack":
				info.AcknowledgementID = configOperationID(999)
			case "backup":
				info.BackupServiceID = configOperationID(999)
			case "replaced":
				info.ReplacedServiceID = ""
			case "manifest":
				info.ManifestDigest = strings.Repeat("d", 64)
			case "context":
				info.ContextRevision = strings.Repeat("d", 64)
			case "store":
				info.StoreDigest = strings.Repeat("d", 64)
			case "token":
				token = strings.Repeat("d", 64)
			case "rotated":
				_, err = f.s.db.Exec("UPDATE nodes SET token_sha256=? WHERE id=?", strings.Repeat("d", 64), node.ID)
			case "audit":
				_, err = f.s.db.Exec("CREATE TRIGGER reject_completion_audit BEFORE INSERT ON config_audit_entries WHEN NEW.code='restore_confirmed' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END")
			}
			if err != nil {
				t.Fatal(err)
			}
			err = f.s.ObserveConfigRestoreConfirmed(context.Background(), node.ID, service, token, info)
			if err == nil {
				t.Fatal("unsafe completion accepted")
			}
			if which == "token" && !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			var n int
			if err := f.s.db.QueryRow("SELECT count(*) FROM config_restore_completions").Scan(&n); err != nil || n != 0 {
				t.Fatal("failed completion retained proof", n, err)
			}
		})
	}
}
