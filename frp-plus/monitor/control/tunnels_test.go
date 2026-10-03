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

const tunnelTestEpoch = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const tunnelTestCollector = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func testTunnelObject(id string) shared.TunnelObject {
	return shared.TunnelObject{InstanceID: id, Identity: shared.TunnelIdentity{ServerID: "server", User: "tenant", RawClientID: ptr("client"), RawName: "web", Kind: "proxy", Protocol: "tcp", Quality: "stable"}, State: shared.TunnelState{Status: "registered", LocalState: "unknown", RemoteState: "unknown", P2PState: "unknown", FallbackState: "unknown"}, Counters: shared.TunnelCounters{Quality: "ok", ConnectionsQuality: "ok", Epoch: tunnelTestEpoch, Accounting: "connection_close", ByteScope: "forwarded_stream", Connections: ptr("1"), RXBytes: ptr("9007199254740993"), TXBytes: ptr("3")}, CreatedAtMS: 1791000000000, OperationRelation: "none"}
}
func testTunnelBatch(o shared.TunnelObject) TunnelBatch {
	return TunnelBatch{CollectorEpoch: tunnelTestCollector, ReceivedAtMS: 1791000001000, Snapshot: shared.TunnelSnapshot{State: "ready", Source: "server", ProcessEpoch: tunnelTestEpoch, CollectedAtMS: 1791000000000, FirstSequence: "1", LastSequence: "1", DroppedEvents: "0", Objects: []shared.TunnelObject{o}, Events: []shared.TunnelEvent{{Sequence: "1", TimeBasis: "native", OccurredAtMS: 1791000000000, Code: "registered", Object: o}}}}
}
func tunnelCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.call(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow("SELECT count(*) FROM " + table).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestTunnelReplayStateWritesAndExactCounters(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	ctx := context.Background()
	o := testTunnelObject("11111111-1111-4111-8111-111111111111")
	b := testTunnelBatch(o)
	mappings, err := f.s.IngestTunnels(ctx, b)
	if err != nil || len(mappings) != 1 {
		t.Fatal(mappings, err)
	}
	events := tunnelCount(t, f.s, "tunnel_events")
	b.Snapshot.Objects[0].Counters.RXBytes = ptr("18446744073709551615")
	b.ReceivedAtMS++
	if _, err = f.s.IngestTunnels(ctx, b); err != nil {
		t.Fatal(err)
	}
	if tunnelCount(t, f.s, "tunnel_events") != events {
		t.Fatal("replay wrote events")
	}
	list, err := f.s.ListTunnels(ctx, TunnelFilter{}, "", 50)
	if err != nil || len(list.Items) != 1 {
		t.Fatal(list, err)
	}
	v := list.Items[0].CurrentInstances[0]
	if *v.Counters.RXBytes != "9007199254740993" || v.Freshness != "stale" {
		t.Fatal("ordinary sample overwrote transition snapshot", v)
	}
	// A reused native ID cannot mutate its authenticated origin, even when that
	// would otherwise create a plausible new logical tunnel.
	b.Snapshot.Objects[0].Identity.RawName = "different"
	b.Snapshot.Events = []shared.TunnelEvent{}
	b.Snapshot.FirstSequence = "0"
	b.Snapshot.LastSequence = "0"
	if _, err = f.s.IngestTunnels(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatal("origin/sequence regression accepted", err)
	}
	if tunnelCount(t, f.s, "tunnels") != 1 {
		t.Fatal("failed frame left orphan tunnel")
	}
}
func TestTunnelBindingEpochSurvivesABAAndDeletedNodes(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	ctx := context.Background()
	n := f.create(DefaultNodeConfig("one"))
	binding := &shared.FRPBinding{ServerID: "server", User: "tenant", RawClientID: "client"}
	if err := f.s.SetBinding(ctx, n.ID, binding, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	b := testTunnelBatch(testTunnelObject("11111111-1111-4111-8111-111111111111"))
	first, err := f.s.IngestTunnels(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = f.s.Get(ctx, n.ID)
	if err = f.s.SetBinding(ctx, n.ID, nil, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	n, _ = f.s.Get(ctx, n.ID)
	if err = f.s.SetBinding(ctx, n.ID, binding, n.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	second, err := f.s.IngestTunnels(ctx, b)
	if err != nil || first[0].TunnelID == second[0].TunnelID || first[0].InstanceID == second[0].InstanceID {
		t.Fatal("ABA rebind merged history", first, second, err)
	}
	if err = f.s.DeleteNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	third, err := f.s.IngestTunnels(ctx, b)
	if err != nil || third[0].TunnelID == second[0].TunnelID {
		t.Fatal("deleted owner reused", third, err)
	}
	page, err := f.s.ListTunnels(ctx, TunnelFilter{NodeID: n.ID}, "", 50)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("deleted node history lost", page, err)
	}
}
func TestTunnelGenerationGapPaginationAndOrigin(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	ctx := context.Background()
	b := testTunnelBatch(testTunnelObject("11111111-1111-4111-8111-111111111111"))
	a, err := f.s.IngestTunnels(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	b.Snapshot.Objects[0].InstanceID = "22222222-2222-4222-8222-222222222222"
	b.Snapshot.Events[0].Object = b.Snapshot.Objects[0]
	b.Snapshot.Events[0].Sequence = "7"
	b.Snapshot.FirstSequence = "7"
	b.Snapshot.LastSequence = "7"
	b.CollectorEpoch = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	z, err := f.s.IngestTunnels(ctx, b)
	if err != nil || a[0].TunnelID != z[0].TunnelID || z[0].Generation != "2" {
		t.Fatal(a, z, err)
	}
	page, err := f.s.ListTunnelEvents(ctx, a[0].TunnelID, TunnelFilter{}, "", 2)
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	next, err := f.s.ListTunnelEvents(ctx, a[0].TunnelID, TunnelFilter{}, page.NextCursor, 200)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	seen := map[string]bool{}
	for _, e := range append(page.Items, next.Items...) {
		if seen[e.EventID] {
			t.Fatal("duplicate cursor item")
		}
		seen[e.EventID] = true
		codes[e.Code] = true
	}
	if !codes["event_gap"] || !codes["collector_gap"] {
		t.Fatal(codes)
	}
	if _, err = f.s.ListTunnelEvents(ctx, a[0].TunnelID, TunnelFilter{InstanceID: z[0].InstanceID}, page.NextCursor, 20); !errors.Is(err, ErrInvalid) {
		t.Fatal("cursor filter changed", err)
	}
	b.Snapshot.Events = []shared.TunnelEvent{}
	b.Snapshot.FirstSequence = "0"
	b.Snapshot.LastSequence = "0"
	if _, err = f.s.IngestTunnels(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatal("sequence rewind accepted", err)
	}
	// Inventory-only new process, preserving logical ID but a new generation.
	b.Snapshot.ProcessEpoch = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	x, err := f.s.IngestTunnels(ctx, b)
	if err != nil || x[0].Generation != "3" {
		t.Fatal(x, err)
	}
	b.Snapshot.Objects[0].CreatedAtMS++
	if _, err = f.s.IngestTunnels(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatal("immutable origin mutation", err)
	}
}
func TestTunnelAgentClaimsTokenRevocationAndPrivateScope(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	ctx := context.Background()
	n := f.create(DefaultNodeConfig("agent"))
	o := testTunnelObject("11111111-1111-4111-8111-111111111111")
	o.Counters = shared.TunnelCounters{Quality: "unsupported", ConnectionsQuality: "unsupported", Accounting: "not_observed", ByteScope: "not_observed"}
	b := testTunnelBatch(o)
	b.NodeID = n.ID
	b.TokenSHA256 = n.TokenSHA256
	b.Snapshot.Source = "client"
	m, err := f.s.IngestTunnels(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.ListTunnels(ctx, TunnelFilter{}, "", 50)
	if err != nil || p.Items[0].IdentityQuality != "instance_only" || *p.Items[0].NodeID != n.ID {
		t.Fatal(p, err)
	}
	if err = f.s.RotateToken(ctx, n.ID, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.IngestTunnels(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatal("revoked source wrote history", err)
	}
	if _, err = f.s.GetTunnelInstance(ctx, m[0].TunnelID, "999"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestTunnelRetentionBoundedAndNoCascade(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	ctx := context.Background()
	b := testTunnelBatch(testTunnelObject("11111111-1111-4111-8111-111111111111"))
	m, err := f.s.IngestTunnels(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.call(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 300; i++ {
			if _, err := tx.Exec(`INSERT INTO tunnel_events(source_key,process_epoch,code,time_basis,received_at_ms)VALUES('server',?,'collector_gap','observed',?)`, tunnelTestEpoch, b.ReceivedAtMS); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(31 * 24 * time.Hour / time.Millisecond))
	b.ReceivedAtMS = f.clock.Load()
	if _, err = f.s.IngestTunnels(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = f.s.PruneTunnels(ctx); err != nil {
		t.Fatal(err)
	}
	if n := tunnelCount(t, f.s, "tunnel_events"); n != 46 {
		t.Fatal("not a bounded 256 deletion", n)
	}
	if tunnelCount(t, f.s, "tunnel_instances") != 1 {
		t.Fatal("current instance removed")
	}
	page, err := f.s.ListTunnelEvents(ctx, m[0].TunnelID, TunnelFilter{}, "", 200)
	if err != nil || !page.EventsPruned || page.RetainedFromMS == 0 {
		t.Fatal(page, err)
	}
}

func TestTunnelSliceBoundsBootstrapWithoutLosingProgress(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	b := testTunnelBatch(testTunnelObject("11111111-1111-4111-8111-111111111111"))
	b.Snapshot.Events = []shared.TunnelEvent{}
	b.Snapshot.FirstSequence = "0"
	b.Snapshot.LastSequence = "0"
	b.Snapshot.Objects = []shared.TunnelObject{}
	for i := 1; i <= 200; i++ {
		o := testTunnelObject(fmt.Sprintf("%08x-1111-4111-8111-111111111111", i))
		o.Identity.RawName = fmt.Sprintf("proxy-%d", i)
		b.Snapshot.Objects = append(b.Snapshot.Objects, o)
	}
	offset, total := 0, 0
	for n := 0; n < 4; n++ {
		r, err := f.s.IngestTunnelSlice(context.Background(), b, offset, 64)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Mappings) > 64 || r.NextObject-offset > 64 {
			t.Fatal("transaction exceeded bound", r)
		}
		offset = r.NextObject
		total += len(r.Mappings)
		if r.Complete != (n == 3) {
			t.Fatal("incorrect completion", r.Complete, n)
		}
	}
	if total != 200 || tunnelCount(t, f.s, "tunnel_instances") != 200 || tunnelCount(t, f.s, "tunnel_events") != 1 {
		t.Fatal("partial bootstrap lost/duplicated instances")
	}
}
func TestTunnelUnavailableDoesNotRewindSequenceOrInventClose(t *testing.T) {
	f := setup(t, time.UnixMilli(1791000001000), time.UTC)
	b := testTunnelBatch(testTunnelObject("11111111-1111-4111-8111-111111111111"))
	m, err := f.s.IngestTunnels(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	original := b
	b.Snapshot = shared.EmptyTunnelSnapshot("server", "unavailable")
	b.Snapshot.ProcessEpoch = tunnelTestEpoch
	b.Snapshot.CollectedAtMS = b.ReceivedAtMS
	if _, err = f.s.IngestTunnels(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.IngestTunnels(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.ListTunnelEvents(context.Background(), m[0].TunnelID, TunnelFilter{}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	created, unavailable := 0, 0
	for _, e := range page.Items {
		switch e.Code {
		case "registered":
			created++
		case "source_unavailable":
			unavailable++
		case "closed":
			t.Fatal("unavailability invented close")
		}
	}
	if created != 1 || unavailable != 1 {
		t.Fatal(created, unavailable)
	}
}
