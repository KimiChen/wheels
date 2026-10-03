//go:build linux || darwin

package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/managed"
	"github.com/fatedier/frp/extension/frpmonitor/shared"
)

type bridgeFixture struct {
	provider     *Provider
	engine       *managed.Engine
	input        Input
	mu           sync.Mutex
	runtime      managed.StoreSnapshot
	applications int
}

func makeBridgeFixture(t *testing.T) *bridgeFixture {
	t.Helper()
	f := &bridgeFixture{input: fixture(t, "", nil)}
	engine, err := managed.Open(managed.Options{Root: f.input.WorkingDir, StorePath: f.input.StoreFile})
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	t.Cleanup(func() { _ = engine.Close() })
	data, _ := os.ReadFile(f.input.StoreFile)
	f.runtime = managed.StoreSnapshot{Exists: true, Bytes: data}
	input := func() (Input, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.input, nil }
	if err = engine.AttachRuntime(managed.Runtime{
		Check: func(ctx context.Context, op managed.Operation, phase string) error {
			in, _ := input()
			if phase == "apply" {
				view, err := Inspect(in)
				if err != nil || view.Revision != op.BaseRevision {
					return errors.New("synthetic native mismatch")
				}
			}
			revision, err := InspectContext(in)
			if err != nil || revision != op.ContextRevision {
				return errors.New("synthetic native context mismatch")
			}
			return nil
		},
		Apply: func(ctx context.Context, snapshot managed.StoreSnapshot) error {
			objects, err := parseStore(snapshot.Bytes)
			if err != nil {
				return err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.input.StoreMemory = native(objects)
			f.runtime = snapshot
			f.applications++
			return nil
		},
		Verify: func(ctx context.Context, snapshot managed.StoreSnapshot) (managed.Verification, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			loaded := managed.Digest(snapshot) == managed.Digest(f.runtime)
			return managed.Verification{RuntimeLoaded: loaded, ResourcesReady: loaded}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.provider, err = NewProvider(engine, input)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func bridgeCommand(action, serviceID string) shared.ConfigCommand {
	id, _ := shared.NewConfigOperationID()
	now := time.Now().UTC()
	return shared.ConfigCommand{Meta: shared.Meta{Schema: 1, SessionID: strings.Repeat("a", 48), Sequence: 2, CollectedAt: now.Format(time.RFC3339Nano)},
		RequestID: id, ServiceID: serviceID, Action: action, DeadlineAtMS: now.Add(10 * time.Second).UnixMilli(), Changes: []shared.ConfigChange{}}
}

func bridgeAction(action string, prepared shared.ConfigResult) shared.ConfigCommand {
	command := bridgeCommand(action, prepared.ServiceID)
	command.OperationID = prepared.Operation.OperationID
	command.BaseRevision = prepared.Operation.BaseRevision
	command.ContextRevision = prepared.Operation.ContextRevision
	command.CandidateDigest = prepared.Operation.CandidateDigest
	return command
}

func TestProviderCompletePrivateStoreCycle(t *testing.T) {
	f := makeBridgeFixture(t)
	ctx := context.Background()
	inspection := f.provider.HandleConfig(ctx, bridgeCommand("inspect", ""))
	if inspection.Code != "ok" || inspection.Validate() != nil || inspection.Inventory.State != "ready" {
		t.Fatalf("inspection: %#v", inspection)
	}
	secret := bridgeCommand("secret", inspection.ServiceID)
	secret.Secret = &shared.ConfigSecretInput{Reference: secretID, Value: "synthetic-native-secret"}
	written := f.provider.HandleConfig(ctx, secret)
	if written.Code != "ok" || written.SecretReference != secretID {
		t.Fatalf("secret upload: %#v", written)
	}
	command := bridgeCommand("prepare", inspection.ServiceID)
	command.OperationID, _ = shared.NewConfigOperationID()
	command.IdempotencyKey, _ = shared.NewConfigOperationID()
	command.BaseRevision = inspection.Inventory.Revision
	command.OperationDeadlineAtMS = time.Now().Add(time.Minute).UnixMilli()
	command.Changes = []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "private-service", Type: "stcp",
		Fields:  []shared.ConfigFieldPatch{{Path: "localPort", Value: json.RawMessage(`8080`)}},
		Secrets: []shared.ConfigSecretPatch{{Path: "secretKey", Mode: "reference", Reference: secretID}}}}
	before, _ := os.ReadFile(f.input.StoreFile)
	prepared := f.provider.HandleConfig(ctx, command)
	if prepared.Code != "ok" || prepared.Validate() != nil || prepared.Preview == nil || prepared.Operation.State != "prepared" {
		t.Fatalf("prepare: %#v", prepared)
	}
	after, _ := os.ReadFile(f.input.StoreFile)
	if string(before) != string(after) || f.applications != 0 {
		t.Fatal("preview changed native state")
	}
	for _, reply := range []shared.ConfigResult{written, prepared} {
		data, _ := json.Marshal(reply)
		if strings.Contains(string(data), secret.Secret.Value) || strings.Contains(string(data), f.input.StoreFile) {
			t.Fatal("private material in safe response")
		}
		wire, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "config.result", "params": reply})
		if _, err := shared.DecodeFrame(wire); err != nil {
			t.Fatal("projection violates actual wire decoder", err)
		}
	}
	applied := f.provider.HandleConfig(ctx, bridgeAction("apply", prepared))
	if applied.Code != "ok" || applied.Operation.State != "confirmed" || !applied.Operation.StorePersisted || !applied.Operation.RuntimeLoaded || !applied.Operation.ResourcesReady || applied.Operation.BusinessChecked {
		t.Fatalf("application: %#v", applied)
	}
	disk, _ := os.ReadFile(f.input.StoreFile)
	if !strings.Contains(string(disk), secret.Secret.Value) {
		t.Fatal("local reference was not resolved into private native Store")
	}
	replayed := f.provider.HandleConfig(ctx, command)
	if replayed.Code != "ok" || replayed.Operation.State != "confirmed" || replayed.Preview != nil || f.applications != 1 {
		t.Fatal("completed prepare replay reapplied or recreated preview")
	}
	bad := command
	bad.Changes = append([]shared.ConfigChange{}, command.Changes...)
	bad.Changes[0].Name = "different-intent"
	if f.provider.HandleConfig(ctx, bad).Code != "conflict" {
		t.Fatal("reused operation accepted different edit intent")
	}
	rolled := f.provider.HandleConfig(ctx, bridgeAction("rollback", prepared))
	if rolled.Code != "ok" || rolled.Operation.State != "rolled_back" || rolled.Operation.StorePersisted || rolled.Operation.RuntimeApplied || !rolled.Operation.RuntimeLoaded {
		t.Fatalf("rollback: %#v", rolled)
	}
	restored, _ := os.ReadFile(f.input.StoreFile)
	if string(restored) != string(before) || f.applications != 2 {
		t.Fatal("rollback failed to restore exact Store")
	}
	if result := f.provider.HandleConfig(ctx, bridgeAction("rollback", prepared)); result.Code != "ok" || f.applications != 2 {
		t.Fatal("rollback replay executed twice")
	}
}

func TestProviderRejectsStaleIdentityRevisionAndInvalidCandidate(t *testing.T) {
	f := makeBridgeFixture(t)
	ctx := context.Background()
	inspection := f.provider.HandleConfig(ctx, bridgeCommand("inspect", ""))
	command := bridgeCommand("prepare", inspection.ServiceID)
	command.OperationID, _ = shared.NewConfigOperationID()
	command.IdempotencyKey, _ = shared.NewConfigOperationID()
	command.BaseRevision = inspection.Inventory.Revision
	command.OperationDeadlineAtMS = time.Now().Add(time.Minute).UnixMilli()
	command.Changes = []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "sample", Type: "tcp", Fields: []shared.ConfigFieldPatch{{Path: "localPort", Value: json.RawMessage(`0`)}}, Secrets: []shared.ConfigSecretPatch{}}}
	if reply := f.provider.HandleConfig(ctx, command); reply.Code != "invalid_field" {
		t.Fatalf("invalid candidate not rejected: %#v", reply)
	}
	command.Changes[0].Fields[0].Value = json.RawMessage(`8080`)
	wrongService := command
	wrongService.ServiceID, _ = shared.NewConfigOperationID()
	if f.provider.HandleConfig(ctx, wrongService).Code != "service_mismatch" {
		t.Fatal("wrong service identity accepted")
	}
	data, _ := os.ReadFile(f.input.StoreFile)
	if err := os.WriteFile(f.input.StoreFile, append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if reply := f.provider.HandleConfig(ctx, command); reply.Code != "revision_conflict" {
		t.Fatalf("stale revision accepted: %#v", reply)
	}
	if f.applications != 0 {
		t.Fatal("invalid commands caused runtime writes")
	}
	command.DeadlineAtMS = time.Now().Add(-time.Second).UnixMilli()
	if f.provider.HandleConfig(ctx, command).Code != "invalid_request" {
		t.Fatal("expired command accepted")
	}
}

func TestProviderHistoricalRollbackReturnsConflictWithHistory(t *testing.T) {
	f := makeBridgeFixture(t)
	ctx := context.Background()
	applyChange := func(operation, name string, port int) shared.ConfigResult {
		inspection := f.provider.HandleConfig(ctx, bridgeCommand("inspect", ""))
		cmd := bridgeCommand("prepare", inspection.ServiceID)
		cmd.OperationID, _ = shared.NewConfigOperationID()
		cmd.IdempotencyKey, _ = shared.NewConfigOperationID()
		cmd.BaseRevision = inspection.Inventory.Revision
		cmd.OperationDeadlineAtMS = time.Now().Add(time.Minute).UnixMilli()
		raw, _ := json.Marshal(port)
		change := shared.ConfigChange{Operation: operation, Kind: "proxy", Name: name, Fields: []shared.ConfigFieldPatch{{Path: "localPort", Value: raw}}, Secrets: []shared.ConfigSecretPatch{}}
		if operation == "create" {
			change.Type = "tcp"
		}
		cmd.Changes = []shared.ConfigChange{change}
		prepared := f.provider.HandleConfig(ctx, cmd)
		if prepared.Code != "ok" {
			t.Fatal("prepare", prepared.Code)
		}
		applied := f.provider.HandleConfig(ctx, bridgeAction("apply", prepared))
		if applied.Code != "ok" || applied.Operation.State != "confirmed" {
			t.Fatal("apply", applied.Code)
		}
		return applied
	}
	first := applyChange("create", "sample", 8080)
	_ = applyChange("update", "sample", 8081)
	diskBefore, _ := os.ReadFile(f.input.StoreFile)
	calls := f.applications
	refused := f.provider.HandleConfig(ctx, bridgeAction("rollback", first))
	if refused.Code != "conflict" || refused.Validate() != nil || refused.Operation == nil || *refused.Operation != *first.Operation {
		t.Fatalf("rollback conflict hidden behind history: %#v", refused)
	}
	diskAfter, _ := os.ReadFile(f.input.StoreFile)
	if string(diskBefore) != string(diskAfter) || f.applications != calls {
		t.Fatal("obsolete rollback mutated state")
	}
	_ = applyChange("update", "sample", 8082)
}

func TestProviderContextRollbackReturnsConflictWithHistory(t *testing.T) {
	f := makeBridgeFixture(t)
	ctx := context.Background()
	inspection := f.provider.HandleConfig(ctx, bridgeCommand("inspect", ""))
	cmd := bridgeCommand("prepare", inspection.ServiceID)
	cmd.OperationID, _ = shared.NewConfigOperationID()
	cmd.IdempotencyKey, _ = shared.NewConfigOperationID()
	cmd.BaseRevision = inspection.Inventory.Revision
	cmd.OperationDeadlineAtMS = time.Now().Add(time.Minute).UnixMilli()
	cmd.Changes = []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "sample", Type: "tcp", Fields: []shared.ConfigFieldPatch{{Path: "localPort", Value: json.RawMessage(`8080`)}}, Secrets: []shared.ConfigSecretPatch{}}}
	prepared := f.provider.HandleConfig(ctx, cmd)
	applied := f.provider.HandleConfig(ctx, bridgeAction("apply", prepared))
	if applied.Code != "ok" {
		t.Fatal(applied.Code)
	}
	f.mu.Lock()
	startup, err := commonCopy(f.input.StartupCommon)
	if err != nil {
		t.Fatal(err)
	}
	startup.Log.Level = "warn"
	f.input.StartupCommon = startup
	f.mu.Unlock()
	refused := f.provider.HandleConfig(ctx, bridgeAction("rollback", applied))
	if refused.Code != "conflict" || refused.Operation == nil || *refused.Operation != *applied.Operation || refused.Validate() != nil {
		t.Fatalf("context refusal hidden: %#v", refused)
	}
	if f.applications != 1 {
		t.Fatal("context refusal touched runtime")
	}
}

func TestProviderInputErrorKeepsSafeBusyClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{{"busy", managed.ErrBusy, "busy"}, {"private", errors.New("synthetic-sensitive-path-and-secret"), "unavailable"}} {
		t.Run(test.name, func(t *testing.T) {
			f := makeBridgeFixture(t)
			inspection := f.provider.HandleConfig(context.Background(), bridgeCommand("inspect", ""))
			f.provider.input = func() (Input, error) { return Input{}, test.err }
			for _, action := range []string{"inspect", "prepare"} {
				command := bridgeCommand(action, f.engine.ServiceID())
				if action == "prepare" {
					command.OperationID, _ = shared.NewConfigOperationID()
					command.IdempotencyKey, _ = shared.NewConfigOperationID()
					command.BaseRevision = inspection.Inventory.Revision
					command.OperationDeadlineAtMS = time.Now().Add(time.Minute).UnixMilli()
					command.Changes = []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "busy-check", Type: "tcp", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{}}}
				}
				result := f.provider.HandleConfig(context.Background(), command)
				if result.Code != test.code || result.Validate() != nil {
					t.Fatal("input failure lost safe classification")
				}
			}
		})
	}
}

func TestOperationViewRetainsExpiredMaterialFactAndCapacityCode(t *testing.T) {
	now := time.Now().UTC()
	expired := now.Add(30 * 24 * time.Hour)
	op := managed.Operation{ID: secretID, BaseRevision: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), OldDigest: strings.Repeat("c", 64), NewDigest: strings.Repeat("d", 64), State: "confirmed", CreatedAt: now, UpdatedAt: now, Deadline: now.Add(time.Minute), StorePersisted: true, RuntimeApplied: true, Verification: managed.Verification{RuntimeLoaded: true, ResourcesReady: true}, MaterialsState: managed.MaterialsExpired, MaterialsExpiredAt: &expired, MaterialsExpiryReason: "ttl"}
	view := operationView(op)
	if view.Validate() != nil || view.State != "confirmed" || view.MaterialsState != "expired" || view.MaterialsExpiredAtMS == nil || *view.MaterialsExpiredAtMS != expired.UnixMilli() || !view.StorePersisted || !view.RuntimeLoaded {
		t.Fatal("projection changed historical facts", view)
	}
	if resultCode(managed.ErrNotFound) != "operation_not_found" || resultCode(managed.ErrCapacity) != "managed_capacity" {
		t.Fatal("bounded local failure was hidden")
	}
}
