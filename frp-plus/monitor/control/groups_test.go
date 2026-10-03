package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

func groupList(t *testing.T, s *Store) []NodeGroup {
	t.Helper()
	groups, err := s.Groups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if groups == nil {
		t.Fatal("groups must be an empty array, never null")
	}
	return groups
}

func newGroup(t *testing.T, s *Store, name string, members ...string) *NodeGroup {
	t.Helper()
	g, err := s.CreateGroup(context.Background(), name, members)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGroupsManyToManyRevisionsAndRestart(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	ctx := context.Background()
	for _, name := range []string{"first", "second", "third"} {
		f.create(DefaultNodeConfig(name))
	}
	if groups := groupList(t, f.s); len(groups) != 0 {
		t.Fatal(groups)
	}
	a := newGroup(t, f.s, "  Production  ", "3", "1")
	b := newGroup(t, f.s, "production", "2", "1") // Names are case-sensitive.
	empty := newGroup(t, f.s, "empty")
	want := []NodeGroup{{a.ID, "Production", []string{"1", "3"}, 1}, {b.ID, "production", []string{"1", "2"}, 1}, {empty.ID, "empty", []string{}, 1}}
	if got := groupList(t, f.s); !reflect.DeepEqual(got, want) {
		t.Fatal(got, want)
	}
	updated, err := f.s.UpdateGroup(ctx, a.ID, "  renamed ", []string{"3", "2"}, a.ConfigRevision)
	if err != nil || updated.Name != "renamed" || updated.ConfigRevision != 2 || !reflect.DeepEqual(updated.NodeIDs, []string{"2", "3"}) {
		t.Fatal(updated, err)
	}
	for _, revision := range []int64{0, -1, 1} {
		if _, err := f.s.UpdateGroup(ctx, a.ID, "stale", nil, revision); !errors.Is(err, ErrConflict) {
			t.Fatal("stale edit accepted", revision, err)
		}
		if err := f.s.DeleteGroup(ctx, a.ID, revision); !errors.Is(err, ErrConflict) {
			t.Fatal("stale delete accepted", revision, err)
		}
	}
	want[0] = *updated
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := groupList(t, f.s); !reflect.DeepEqual(got, want) {
		t.Fatal("groups changed after restart", got)
	}
	if err := f.s.DeleteGroup(ctx, b.ID, b.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Get(ctx, "1"); err != nil {
		t.Fatal("deleting a group removed its member", err)
	}
	if err := f.s.DeleteGroup(ctx, empty.ID, empty.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if g := newGroup(t, f.s, "next"); g.ID != "4" {
		t.Fatal("deleted group ID reused", g)
	}
	if err := f.s.call(ctx, func(tx *sql.Tx) error {
		var dangling int
		if err := tx.QueryRow("SELECT count(*) FROM node_group_members WHERE group_id=?", b.ID).Scan(&dangling); err != nil {
			return err
		}
		if dangling != 0 {
			return fmt.Errorf("group deletion left %d memberships", dangling)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGroupValidationAndAtomicReplacement(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	ctx := context.Background()
	f.create(DefaultNodeConfig("one"))
	f.create(DefaultNodeConfig("two"))
	g := newGroup(t, f.s, "original", "1")
	newGroup(t, f.s, "other", "2")
	before := groupList(t, f.s)
	for _, name := range []string{"", " \u2003 ", "line\nbreak", "\tlabel", "a\x00b", "a\u0085b", "bad\xff", strings.Repeat("界", 43), strings.Repeat("x", 129)} {
		if _, err := f.s.CreateGroup(ctx, name, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid name %q accepted: %v", name, err)
		}
	}
	for _, members := range [][]string{{"1", "1"}, {"0"}, {"01"}, {"-1"}, {"1.0"}, {"9223372036854775808"}, {"3"}, make([]string, 1025)} {
		if _, err := f.s.UpdateGroup(ctx, g.ID, "changed", members, g.ConfigRevision); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid membership accepted", members, err)
		}
	}
	if _, err := f.s.CreateGroup(ctx, " original ", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate name accepted", err)
	}
	if _, err := f.s.UpdateGroup(ctx, g.ID, "other", []string{"2"}, g.ConfigRevision); !errors.Is(err, ErrConflict) {
		t.Fatal("renamed group to an existing name", err)
	}
	for _, id := range []string{"0", "01", "-1"} {
		if err := f.s.DeleteGroup(ctx, id, 1); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid group ID accepted", err)
		}
	}
	if _, err := f.s.UpdateGroup(ctx, "999", "missing", nil, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown group did not return not found", err)
	}
	if got := groupList(t, f.s); !reflect.DeepEqual(got, before) {
		t.Fatal("rejected input changed stored groups", got)
	}
	// Fail after the renamed group and its first new membership were written.
	if err := f.s.call(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_group_member BEFORE INSERT ON node_group_members
 WHEN NEW.node_id=2 BEGIN SELECT RAISE(ABORT,'test membership failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateGroup(ctx, g.ID, "changed", []string{"1", "2"}, g.ConfigRevision); err == nil {
		t.Fatal("injected membership failure ignored")
	}
	if _, err := f.s.CreateGroup(ctx, "new", []string{"1", "2"}); err == nil {
		t.Fatal("injected create failure ignored")
	}
	if got := groupList(t, f.s); !reflect.DeepEqual(got, before) {
		t.Fatal("failed transaction changed groups or their revisions", got)
	}
	if g := newGroup(t, f.s, strings.Repeat("界", 42)+"ab"); len(g.Name) != 128 {
		t.Fatal("128-byte group name rejected", g)
	}
}

func TestGroupLimitsAndNodeDeletionRevisions(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	ctx := context.Background()
	n1 := f.create(DefaultNodeConfig("one"))
	n2 := f.create(DefaultNodeConfig("two"))
	a := newGroup(t, f.s, "both", n1.ID, n2.ID)
	b := newGroup(t, f.s, "first", n1.ID)
	c := newGroup(t, f.s, "second", n2.ID)
	if err := f.s.WriteProbes(ctx, []byte(`{"version":2,"nodes":[{"agent_id":"1","tasks":[]}]}`)); err != nil {
		t.Fatal(err)
	}
	// A later DELETE failure must also roll back revisions and probe cleanup.
	if err := f.s.call(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_node_delete BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT,'test delete failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := groupList(t, f.s)
	if err := f.s.DeleteNode(ctx, n1.ID); err == nil {
		t.Fatal("injected deletion failure ignored")
	}
	if got := groupList(t, f.s); !reflect.DeepEqual(got, before) {
		t.Fatal("failed node deletion changed groups", got)
	}
	data, err := f.s.ReadProbes(ctx)
	if err != nil || !strings.Contains(string(data), `"agent_id":"1"`) {
		t.Fatal("failed deletion changed probes", string(data), err)
	}
	if err := f.s.call(ctx, func(tx *sql.Tx) error { _, err := tx.Exec("DROP TRIGGER reject_node_delete"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteNode(ctx, n1.ID); err != nil {
		t.Fatal(err)
	}
	want := []NodeGroup{{a.ID, a.Name, []string{n2.ID}, 2}, {b.ID, b.Name, []string{}, 2}, *c}
	if got := groupList(t, f.s); !reflect.DeepEqual(got, want) {
		t.Fatal("node deletion did not update group revisions/membership", got)
	}
	if _, err := f.s.UpdateGroup(ctx, a.ID, a.Name, []string{n2.ID}, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("editor predating node deletion was accepted", err)
	}
	if err := f.s.call(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE ids(n) AS (SELECT 4 UNION ALL SELECT n+1 FROM ids WHERE n<127)
 INSERT INTO node_groups(id,name) SELECT n,printf('group-%d',n) FROM ids`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	last := newGroup(t, f.s, "last")
	if last.ID != "128" {
		t.Fatal(last)
	}
	if _, err := f.s.CreateGroup(ctx, "overflow", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("group cap did not return conflict", err)
	}
	if err := f.s.DeleteGroup(ctx, last.ID, last.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if next := newGroup(t, f.s, "replacement"); next.ID != "129" {
		t.Fatal("group ID reused after capacity recovered", next)
	}
}

func TestGroupAcceptsAll1024Nodes(t *testing.T) {
	f := setup(t, utc("2026-09-28T12:00:00Z"), time.UTC)
	if err := f.s.call(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<1024)
 INSERT INTO nodes(name,token_sha256,created_at_ms,updated_at_ms) SELECT 'fixture',printf('%064x',n),0,0 FROM ids`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 1024)
	for i := range ids {
		ids[i] = fmt.Sprint(1024 - i)
	}
	g := newGroup(t, f.s, "all", ids...)
	if len(g.NodeIDs) != 1024 || g.NodeIDs[0] != "1" || g.NodeIDs[1023] != "1024" {
		t.Fatal("member limit or numeric ordering is incorrect", g)
	}
}

func legacyDatabase(t *testing.T) (string, *sql.DB) {
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
	// nodes/settings are unchanged from v4; omit only the appended group DDL.
	schema, _, found := strings.Cut(Schema, groupSchemaMarker)
	if !found {
		t.Fatal("v4 schema boundary missing")
	}
	if _, err := db.Exec(strings.Replace(schema, " counter_interface TEXT,", " counter_interface TEXT,\n counter_scope TEXT,", 1) + "PRAGMA application_id=1179798836; PRAGMA user_version=4;"); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestLegacyMigrationPreservesNodeDataAndSequences(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path, db := legacyDatabase(t)
			if version == 5 {
				groups, e := schemaSection(groupSchemaMarker, operationSchemaMarker)
				if e != nil {
					t.Fatal(e)
				}
				if _, err := db.Exec(groups + "PRAGMA user_version=5;"); err != nil {
					t.Fatal(err)
				}
			}
			at := utc("2026-09-28T12:00:00Z")
			n := &Node{ID: "7", NodeConfig: DefaultNodeConfig("existing"), TokenSHA256: strings.Repeat("a", 64),
				Binding:              &shared.FRPBinding{ServerID: "server", User: "tenant", RawClientID: "stable"},
				TrafficPeriodRXBytes: "184467440737095516160", TrafficPeriodTXBytes: "9007199254740993", TrafficAdjustmentBytes: "-123",
				TrafficTodayRXBytes: "9007199254740994", TrafficTodayTXBytes: "9007199254740995", ConfigRevision: 17, CreatedAtMS: 123, UpdatedAtMS: 456,
				CounterBootID: ptr("boot"), CounterInterface: ptr("eth0"), CounterRXBytes: ptr("18446744073709551615"),
				CounterTXBytes: ptr("123456"), CounterReceivedAtMS: ptr(at.UnixMilli())}
			n.PrivateNote, n.PriceMinor, n.Currency = "private note", ptr("1200"), ptr("USD")
			(&Store{cfg: Config{Location: time.UTC}}).refresh(n, at)
			// refresh initializes boundaries; restore the existing accumulated values.
			n.TrafficPeriodRXBytes, n.TrafficPeriodTXBytes, n.TrafficAdjustmentBytes = "184467440737095516160", "9007199254740993", "-123"
			n.TrafficTodayRXBytes, n.TrafficTodayTXBytes = "9007199254740994", "9007199254740995"
			args := nodeArgs(n)
			if _, err := db.Exec("INSERT INTO nodes ("+columns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+")", args...); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO nodes(id,name,token_sha256,created_at_ms,updated_at_ms) VALUES(29,'deleted',?,0,0); DELETE FROM nodes WHERE id=29", strings.Repeat("b", 64)); err != nil {
				t.Fatal(err)
			}
			probes := `{"version":23,"nodes":[{"agent_id":"7","tasks":[]}]}`
			if _, err := db.Exec("UPDATE settings SET probe_json=?", probes); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE nodes SET counter_scope='unknown'"); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Path: path, Location: time.UTC, Now: func() time.Time { return at }}
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			if s.MigrationBackupPath() == "" {
				t.Fatal("legacy migration did not create a recovery image")
			}
			saved, err := sql.Open("sqlite", s.MigrationBackupPath())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { saved.Close() })
			var savedVersion, savedLegacyColumn int
			if err := saved.QueryRow("PRAGMA user_version").Scan(&savedVersion); err != nil || savedVersion != version {
				t.Fatal("backup was not taken before the migration", savedVersion, err)
			}
			if err := saved.QueryRow("SELECT count(*) FROM pragma_table_info('nodes') WHERE name='counter_scope'").Scan(&savedLegacyColumn); err != nil || savedLegacyColumn != 1 {
				t.Fatal("backup lost the legacy baseline column", savedLegacyColumn, err)
			}
			var removed int
			if err := s.db.QueryRow("SELECT count(*) FROM pragma_table_info('nodes') WHERE name='counter_scope'").Scan(&removed); err != nil || removed != 0 {
				t.Fatal("legacy baseline column remains", removed, err)
			}
			actual, err := s.Get(context.Background(), n.ID)
			if err != nil || !reflect.DeepEqual(actual, n) {
				t.Fatal("migration changed node data", actual, n, err)
			}
			if data, err := s.ReadProbes(context.Background()); err != nil || string(data) != probes {
				t.Fatal("migration changed probe settings", string(data), err)
			}
			if len(groupList(t, s)) != 0 {
				t.Fatal("migration invented groups")
			}
			created, err := s.CreateNode(context.Background(), DefaultNodeConfig("next"), strings.Repeat("c", 64), nil)
			if err != nil || created.ID != "30" {
				t.Fatal("migration changed node autoincrement sequence", created, err)
			}
			if _, err := s.CreateNode(context.Background(), DefaultNodeConfig("duplicate binding"), strings.Repeat("d", 64), n.Binding); !errors.Is(err, ErrConflict) {
				t.Fatal("migration removed binding uniqueness", err)
			}
			newGroup(t, s, "migrated", n.ID, created.ID)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			s = reopened
			if len(groupList(t, s)) != 1 {
				t.Fatal("upgraded database did not reopen", err)
			}
			if err := s.call(context.Background(), func(tx *sql.Tx) error {
				var version int
				if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
					return err
				}
				if version != 8 {
					return fmt.Errorf("unexpected migration version %d", version)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationRejectsUnknownIdentityAndRollsBackDDL(t *testing.T) {
	for _, tt := range []struct{ version, application int }{{3, 1179798836}, {9, 1179798836}, {4, 123}, {5, 123}} {
		t.Run(fmt.Sprint(tt), func(t *testing.T) {
			path, db := legacyDatabase(t)
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d; PRAGMA application_id=%d", tt.version, tt.application)); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if s, err := Open(Config{Path: path}); err == nil {
				s.Close()
				t.Fatal("unsupported database accepted")
			}
		})
	}
	path, db := legacyDatabase(t)
	// The second CREATE fails, after node_groups was created in the transaction.
	if _, err := db.Exec("CREATE TABLE node_group_members(existing TEXT)"); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(Config{Path: path}); err == nil {
		s.Close()
		t.Fatal("conflicting v4 schema accepted")
	}
	var version, groups int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='node_groups'").Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if version != 4 || groups != 0 {
		t.Fatal("failed migration left partial schema", version, groups)
	}
}
