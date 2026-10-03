package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestConfirmedRestoreLineageRequiresFullCompletedBackupChain(t *testing.T) {
	f := setup(t, utc("2026-01-01T12:00:00Z"), time.UTC)
	node := f.create(DefaultNodeConfig("lineage"))
	ctx := context.Background()
	original := configOperationID(800)
	add := func(n int, parent string, complete bool) *ConfigRestore {
		req := restoreRequest(node, n)
		req.ServiceID = configOperationID(n + 1000)
		req.Epoch = configOperationID(n + 2000)
		req.BackupServiceID = parent
		r, _, err := f.s.ClaimConfigRestore(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		r, err = f.s.ConfirmConfigRestore(ctx, r.ID, r.Version)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			if err = f.s.ObserveConfigRestoreConfirmed(ctx, node.ID, r.ServiceID, node.TokenSHA256, completedRestoreInfo(r)); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	a := add(901, original, true)
	b := add(902, a.ServiceID, true)
	allowed, err := f.s.ConfirmedRestoreLineage(ctx, node.ID, original, b.ServiceID, node.TokenSHA256)
	if err != nil || !allowed {
		t.Fatal("confirmed chain", allowed, err)
	}
	c := add(903, b.ServiceID, false)
	if allowed, err = f.s.ConfirmedRestoreLineage(ctx, node.ID, original, c.ServiceID, node.TokenSHA256); err != nil || allowed {
		t.Fatal("acknowledged promoted to completion", allowed, err)
	}
	if allowed, _ = f.s.ConfirmedRestoreLineage(ctx, node.ID, b.ReplacedServiceID, b.ServiceID, node.TokenSHA256); allowed {
		t.Fatal("replaced target mistaken for backup lineage")
	}
	if allowed, err = f.s.ConfirmedRestoreLineage(ctx, node.ID, original, b.ServiceID, strings.Repeat("f", 64)); err == nil || allowed {
		t.Fatal("stale binding accepted")
	}
	if allowed, err = f.s.ConfirmedRestoreLineage(ctx, node.ID, original, original, node.TokenSHA256); err != nil || allowed {
		t.Fatal("identity equality mistaken for relocation")
	}
}
