package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTunnelSchemaTenAndElevenMigrationBackups(t *testing.T) {
	for _, version := range []int{10, 11} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "control.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			source := baseSchema
			if version == 10 {
				source, _, _ = strings.Cut(source, tunnelSchemaMarker)
				source += "PRAGMA application_id=1179798836;PRAGMA user_version=10;"
			}
			if _, err = db.Exec(source + `PRAGMA journal_mode=WAL;PRAGMA wal_autocheckpoint=0;`); err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO nodes(name,token_sha256,created_at_ms,updated_at_ms,traffic_period_rx_bytes)VALUES('old',?,1791000000000,1791000000000,'9007199254740993')`, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			defer db.Close()
			if s.MigrationBackupPath() == "" {
				t.Fatal("missing consistent backup")
			}
			saved, err := sql.Open("sqlite", s.MigrationBackupPath())
			if err != nil {
				t.Fatal(err)
			}
			defer saved.Close()
			var before, current int
			var bytes string
			if err = saved.QueryRow(`PRAGMA user_version`).Scan(&before); err != nil || before != version {
				t.Fatal(before, err)
			}
			if err = saved.QueryRow(`SELECT traffic_period_rx_bytes FROM nodes WHERE id=1`).Scan(&bytes); err != nil || bytes != "9007199254740993" {
				t.Fatal("backup missed committed WAL", bytes, err)
			}
			if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil || current != 12 {
				t.Fatal(current, err)
			}
			var retained string
			if err = s.db.QueryRow(`SELECT traffic_period_rx_bytes FROM nodes WHERE id=1`).Scan(&retained); err != nil || retained != "9007199254740993" {
				t.Fatal("migration changed persisted counters", retained, err)
			}
			if tunnelCount(t, s, "tunnel_binding_epochs") != 1 {
				t.Fatal("binding migration missing/duplicated")
			}
		})
	}
}
func TestTunnelMigrationFailureLeavesVersionTenUntouched(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(directory, 0700)
	path := filepath.Join(directory, "control.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, _, _ := strings.Cut(baseSchema, tunnelSchemaMarker)
	if _, err = db.Exec(source + "PRAGMA application_id=1179798836;PRAGMA user_version=10;"); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0600)
	expected := errors.New("backup unavailable")
	_, err = openWithMigrationBackup(Config{Path: path}, func(context.Context, *sql.DB, string, int) (string, error) { return "", expected })
	if !errors.Is(err, expected) {
		t.Fatal(err)
	}
	var version, tables int
	db.QueryRow(`PRAGMA user_version`).Scan(&version)
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='tunnels'`).Scan(&tables)
	if version != 10 || tables != 0 {
		t.Fatal("failed backup mutated schema", version, tables)
	}
}
func TestTunnelOperationLinksRequireActualServiceAndBindingInterval(t *testing.T) {
	f := setup(t, utc("2026-10-03T12:00:00Z"), nil)
	ctx := context.Background()
	n := f.create(DefaultNodeConfig("agent"))
	if err := f.s.SetBinding(ctx, n.ID, &shared.FRPBinding{ServerID: "server", User: "tenant", RawClientID: "client"}, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	req := operationRequest(f, n.ID, 1)
	req.ServiceID = configOperationID(999)
	op := createOperation(t, f.s, req)
	o := testTunnelObject("11111111-1111-4111-8111-111111111111")
	o.ServiceID = req.ServiceID
	o.Counters = shared.TunnelCounters{Quality: "unsupported", ConnectionsQuality: "unsupported", Accounting: "not_observed", ByteScope: "not_observed"}
	b := testTunnelBatch(o)
	b.Snapshot.Source = "client"
	b.NodeID = n.ID
	b.TokenSHA256 = n.TokenSHA256
	b.ReceivedAtMS = f.clock.Load()
	m, err := f.s.IngestTunnels(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.s.ListTunnelEvents(ctx, m[0].TunnelID, TunnelFilter{}, "", 100)
	if err != nil || len(page.OperationLinks) != 1 || page.OperationLinks[0].OperationID != op.OperationID || page.OperationLinks[0].Relation != "declared_change" {
		t.Fatal(page, err)
	}
	for _, e := range page.Items {
		if e.OperationID != "" || e.OperationRelation != "none" {
			t.Fatal("declared change became event causality")
		}
	}
	o.ServiceID = configOperationID(998)
	o.InstanceID = "22222222-2222-4222-8222-222222222222"
	o.Identity.RawName = "unrelated"
	b.Snapshot.Objects = []shared.TunnelObject{o}
	b.Snapshot.Events = []shared.TunnelEvent{}
	b.Snapshot.FirstSequence = "0"
	b.Snapshot.LastSequence = "0"
	b.Snapshot.ProcessEpoch = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	m, err = f.s.IngestTunnels(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	page, err = f.s.ListTunnelEvents(ctx, m[0].TunnelID, TunnelFilter{}, "", 100)
	if err != nil || len(page.OperationLinks) != 0 {
		t.Fatal("cross-service operation link", page, err)
	}
}
