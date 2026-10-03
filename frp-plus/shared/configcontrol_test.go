package shared

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func controlID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }
func controlMeta() Meta {
	return Meta{Schema: SchemaVersion, SessionID: "private-session", Sequence: 2, CollectedAt: "2026-10-03T12:00:00Z"}
}
func controlTime() time.Time { v, _ := time.Parse(time.RFC3339, controlMeta().CollectedAt); return v }
func controlDigest() string  { return strings.Repeat("a", 64) }
func controlCommand(action string) ConfigCommand {
	c := ConfigCommand{Meta: controlMeta(), RequestID: controlID(1), ServiceID: controlID(2), Action: action, DeadlineAtMS: controlTime().Add(30 * time.Second).UnixMilli(), Changes: []ConfigChange{}}
	switch action {
	case "prepare":
		c.OperationID, c.IdempotencyKey, c.BaseRevision = controlID(3), controlID(4), controlDigest()
		c.OperationDeadlineAtMS = controlTime().Add(10 * time.Minute).UnixMilli()
		c.Changes = []ConfigChange{{Operation: "create", Kind: "proxy", Name: "web", Type: "tcp", Fields: []ConfigFieldPatch{{Path: "remotePort", Value: json.RawMessage("8000")}}, Secrets: []ConfigSecretPatch{}}}
	case "apply", "query", "rollback", "cancel":
		c.OperationID, c.BaseRevision = controlID(3), controlDigest()
		if action == "apply" || action == "rollback" {
			c.ContextRevision, c.CandidateDigest = controlDigest(), controlDigest()
		}
	case "secret":
		c.Secret = &ConfigSecretInput{Reference: controlID(5), Value: "synthetic-secret"}
	}
	return c
}
func controlObject() ConfigObjectView {
	return ConfigObjectView{Kind: "proxy", Name: "web", Type: "tcp", Source: "store", Writable: true, Active: true, Fields: []ConfigFieldPatch{{Path: "localPort", Value: json.RawMessage("80")}, {Path: "transport.useEncryption", Value: json.RawMessage("true")}}, Secrets: []ConfigSecretPresence{{Path: "loadBalancer.groupKey", Present: true}}, ReadOnlyFields: []string{}, Issues: []ConfigIssue{}}
}
func controlOperation() *ConfigOperationView {
	return &ConfigOperationView{OperationID: controlID(3), BaseRevision: controlDigest(), ContextRevision: controlDigest(), OldDigest: strings.Repeat("b", 64), CandidateDigest: controlDigest(), State: "prepared", CreatedAtMS: controlTime().UnixMilli(), UpdatedAtMS: controlTime().UnixMilli(), DeadlineAtMS: controlTime().Add(10 * time.Minute).UnixMilli()}
}
func controlResult(action string) ConfigResult {
	r := ConfigResult{Meta: controlMeta(), RequestID: controlID(1), ServiceID: controlID(2), Action: action, Code: "ok"}
	switch action {
	case "inspect":
		r.Inventory = &ConfigInventory{Revision: controlDigest(), ContextRevision: controlDigest(), State: "ready", Objects: []ConfigObjectView{controlObject()}, Dependencies: []ConfigDependency{{Kind: "file", State: "ready"}, {Kind: "store", State: "missing"}}, Issues: []ConfigIssue{}}
	case "secret":
		r.SecretReference = controlID(5)
	default:
		r.OperationID = controlID(3)
		r.Operation = controlOperation()
	}
	if action == "prepare" {
		object := controlObject()
		r.Preview = &ConfigPreview{BaseRevision: controlDigest(), ContextRevision: controlDigest(), CandidateDigest: controlDigest(), Changes: []ConfigChangePreview{{Operation: "create", After: &object}}, Warnings: []ConfigIssue{}}
	}
	return r
}

func TestConfigControlRoundTripAllActions(t *testing.T) {
	for _, action := range []string{"inspect", "prepare", "apply", "query", "rollback", "cancel", "secret"} {
		t.Run(action, func(t *testing.T) {
			command := controlCommand(action)
			if err := command.ValidateAt(controlTime()); err != nil {
				t.Fatal(err)
			}
			frame, err := DecodeFrame(encodeFrame(t, "config.command", command))
			if err != nil || frame.ConfigCommand == nil || frame.ConfigCommand.Action != action || frame.Hello != nil || frame.FRPDetail != nil {
				t.Fatal("command round trip", frame, err)
			}
			result := controlResult(action)
			frame, err = DecodeFrame(encodeFrame(t, "config.result", result))
			if err != nil || frame.ConfigResult == nil || frame.ConfigResult.Action != action || frame.Report != nil {
				t.Fatal("result round trip", frame, err)
			}
		})
	}
	discovery := controlCommand("inspect")
	discovery.ServiceID = ""
	if _, err := DecodeFrame(encodeFrame(t, "config.command", discovery)); err != nil {
		t.Fatal("first inspection must discover service identity", err)
	}
	c := controlCommand("prepare")
	c.Changes[0].Fields[0].Value = json.RawMessage("null")
	if _, err := DecodeFrame(encodeFrame(t, "config.command", c)); err != nil {
		t.Fatal("explicit null field reset rejected", err)
	}
}

func TestConfigCommandRejectsInvalidCombinations(t *testing.T) {
	tests := []struct {
		name, action string
		mutate       func(*ConfigCommand)
	}{
		{"request_identity", "inspect", func(c *ConfigCommand) { c.RequestID = "not-a-uuid" }},
		{"service_identity", "apply", func(c *ConfigCommand) { c.ServiceID = "" }},
		{"session_sequence", "inspect", func(c *ConfigCommand) { c.Sequence = 1 }},
		{"inspect_payload", "inspect", func(c *ConfigCommand) { c.BaseRevision = controlDigest() }},
		{"inspect_secret", "inspect", func(c *ConfigCommand) { c.Secret = controlCommand("secret").Secret }},
		{"secret_operation", "secret", func(c *ConfigCommand) { c.OperationID = controlID(3) }},
		{"secret_reference_path", "secret", func(c *ConfigCommand) { c.Secret.Reference = "/private/credentials" }},
		{"secret_empty", "secret", func(c *ConfigCommand) { c.Secret.Value = "" }},
		{"secret_size", "secret", func(c *ConfigCommand) { c.Secret.Value = strings.Repeat("x", MaxStringBytes+1) }},
		{"secret_nul", "secret", func(c *ConfigCommand) { c.Secret.Value = "x\x00x" }},
		{"secret_cr", "secret", func(c *ConfigCommand) { c.Secret.Value = "x\rx" }},
		{"secret_lf", "secret", func(c *ConfigCommand) { c.Secret.Value = "x\nx" }},
		{"null_changes", "query", func(c *ConfigCommand) { c.Changes = nil }},
		{"prepare_no_changes", "prepare", func(c *ConfigCommand) { c.Changes = []ConfigChange{} }},
		{"prepare_no_idempotency", "prepare", func(c *ConfigCommand) { c.IdempotencyKey = "" }},
		{"prepare_preset_candidate", "prepare", func(c *ConfigCommand) { c.CandidateDigest = controlDigest() }},
		{"prepare_preset_context", "prepare", func(c *ConfigCommand) { c.ContextRevision = controlDigest() }},
		{"prepare_secret", "prepare", func(c *ConfigCommand) { c.Secret = controlCommand("secret").Secret }},
		{"operation_before_command", "prepare", func(c *ConfigCommand) { c.OperationDeadlineAtMS = c.DeadlineAtMS - 1 }},
		{"operation_too_long", "prepare", func(c *ConfigCommand) {
			c.OperationDeadlineAtMS = controlTime().Add(MaxConfigOperationDuration + time.Millisecond).UnixMilli()
		}},
		{"missing_apply_digest", "apply", func(c *ConfigCommand) { c.CandidateDigest = "" }},
		{"missing_rollback_context", "rollback", func(c *ConfigCommand) { c.ContextRevision = "" }},
		{"apply_idempotency", "apply", func(c *ConfigCommand) { c.IdempotencyKey = controlID(4) }},
		{"query_missing_base", "query", func(c *ConfigCommand) { c.BaseRevision = "" }},
		{"cancel_invalid_digest", "cancel", func(c *ConfigCommand) { c.CandidateDigest = "bad" }},
		{"apply_changes", "apply", func(c *ConfigCommand) { c.Changes = controlCommand("prepare").Changes }},
		{"duplicate_path", "prepare", func(c *ConfigCommand) { c.Changes[0].Fields = append(c.Changes[0].Fields, c.Changes[0].Fields[0]) }},
		{"secret_in_fields", "prepare", func(c *ConfigCommand) { c.Changes[0].Fields[0].Path = "httpPassword" }},
		{"nested_object", "prepare", func(c *ConfigCommand) {
			c.Changes[0].Fields[0].Value = json.RawMessage(`{"password":"synthetic-secret"}`)
		}},
		{"unbounded_changes", "prepare", func(c *ConfigCommand) { c.Changes = make([]ConfigChange, MaxConfigChanges+1) }},
		{"expired_relative", "inspect", func(c *ConfigCommand) { c.DeadlineAtMS = controlTime().UnixMilli() }},
		{"command_too_long", "inspect", func(c *ConfigCommand) {
			c.DeadlineAtMS = controlTime().Add(MaxConfigCommandDuration + time.Millisecond).UnixMilli()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := controlCommand(tt.action)
			tt.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("direct validation accepted invalid command")
			}
			if _, err := DecodeFrame(encodeFrame(t, "config.command", c)); err == nil {
				t.Fatal("wire accepted invalid command")
			}
		})
	}
	c := controlCommand("inspect")
	if c.ValidateAt(controlTime().Add(30*time.Second)) == nil {
		t.Fatal("expired command may execute")
	}
	if c.ValidateAt(controlTime().Add(-time.Minute)) == nil {
		t.Fatal("far-future command may execute")
	}
	if c.ValidateAt(controlTime().Add(29*time.Second)) != nil {
		t.Fatal("valid deadline was rejected")
	}
}

func TestConfigResultRejectsLeakageAndContradictions(t *testing.T) {
	tests := []struct {
		name, action string
		mutate       func(*ConfigResult)
	}{
		{"unknown_error", "inspect", func(r *ConfigResult) { r.Code = "native error synthetic-secret" }},
		{"missing_service", "inspect", func(r *ConfigResult) { r.ServiceID = "" }},
		{"error_inventory", "inspect", func(r *ConfigResult) { r.Code = "unsupported" }},
		{"secret_echo", "query", func(r *ConfigResult) { r.SecretReference = controlID(5) }},
		{"mismatched_operation", "query", func(r *ConfigResult) { r.Operation.OperationID = controlID(6) }},
		{"unknown_operation_error", "query", func(r *ConfigResult) { r.Operation.ErrorCode = "path /private/secret" }},
		{"unverified_confirmed", "query", func(r *ConfigResult) { r.Operation.State = "confirmed" }},
		{"false_rollback", "query", func(r *ConfigResult) { r.Operation.State = "rolled_back" }},
		{"invalid_operation_time", "query", func(r *ConfigResult) { r.Operation.UpdatedAtMS = r.Operation.CreatedAtMS - 1 }},
		{"ready_without_loaded", "query", func(r *ConfigResult) { r.Operation.ResourcesReady = true }},
		{"native_field", "inspect", func(r *ConfigResult) { r.Inventory.Objects[0].Fields[0].Path = "plugin.password" }},
		{"native_nested_value", "inspect", func(r *ConfigResult) {
			r.Inventory.Objects[0].Fields[0].Value = json.RawMessage(`{"password":"synthetic-secret"}`)
		}},
		{"duplicate_ready_object", "inspect", func(r *ConfigResult) { r.Inventory.Objects = append(r.Inventory.Objects, r.Inventory.Objects[0]) }},
		{"duplicate_field", "inspect", func(r *ConfigResult) { o := &r.Inventory.Objects[0]; o.Fields = append(o.Fields, o.Fields[0]) }},
		{"duplicate_secret", "inspect", func(r *ConfigResult) { o := &r.Inventory.Objects[0]; o.Secrets = append(o.Secrets, o.Secrets[0]) }},
		{"secret_presence_path", "inspect", func(r *ConfigResult) { r.Inventory.Objects[0].Secrets[0].Path = "arbitrary_secret" }},
		{"unknown_read_only", "inspect", func(r *ConfigResult) {
			r.Inventory.Objects[0].ReadOnlyFields = []string{"unknown native key synthetic-secret"}
		}},
		{"unknown_issue", "inspect", func(r *ConfigResult) { r.Inventory.Issues = []ConfigIssue{{Code: "native error synthetic-secret"}} }},
		{"writable_file", "inspect", func(r *ConfigResult) { r.Inventory.Objects[0].Source = "file" }},
		{"writable_read_only", "inspect", func(r *ConfigResult) { r.Inventory.State = "read_only" }},
		{"writable_conflict", "inspect", func(r *ConfigResult) { r.Inventory.Objects[0].Issues = []ConfigIssue{{Code: "ownership_conflict"}} }},
		{"dependency_path", "inspect", func(r *ConfigResult) { r.Inventory.Dependencies[0].Kind = "/private/cert" }},
		{"dependency_count", "inspect", func(r *ConfigResult) { r.Inventory.Dependencies = make([]ConfigDependency, MaxConfigDependencies+1) }},
		{"object_count", "inspect", func(r *ConfigResult) { r.Inventory.Objects = make([]ConfigObjectView, MaxConfigObjects+1) }},
		{"prepared_without_preview", "prepare", func(r *ConfigResult) { r.Preview = nil }},
		{"preview_read_only", "prepare", func(r *ConfigResult) { r.Preview.Changes[0].After.Writable = false }},
		{"preview_digest", "prepare", func(r *ConfigResult) { r.Preview.CandidateDigest = strings.Repeat("b", 64) }},
		{"preview_create_before", "prepare", func(r *ConfigResult) { r.Preview.Changes[0].Before = r.Preview.Changes[0].After }},
		{"preview_missing_after", "prepare", func(r *ConfigResult) { r.Preview.Changes[0].After = nil }},
		{"preview_duplicate", "prepare", func(r *ConfigResult) { r.Preview.Changes = append(r.Preview.Changes, r.Preview.Changes[0]) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := controlResult(tt.action)
			tt.mutate(&r)
			if r.Validate() == nil {
				t.Fatal("direct validation accepted invalid result")
			}
			if _, err := DecodeFrame(encodeFrame(t, "config.result", r)); err == nil {
				t.Fatal("wire accepted invalid result")
			}
		})
	}
	for _, state := range []string{"outcome_unknown", "rollback_failed", "confirmed", "rolled_back"} {
		r := controlResult("query")
		r.Operation.State = state
		if state == "confirmed" {
			r.Operation.StorePersisted = true
			r.Operation.RuntimeApplied = true
			r.Operation.RuntimeLoaded = true
			r.Operation.ResourcesReady = true
		}
		if state == "rolled_back" {
			r.Operation.RuntimeLoaded = true
			r.Operation.ResourcesReady = true
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("operation fact %s rejected: %v", state, err)
		}
	}
	for _, state := range []string{"applying", "outcome_unknown", "rollback_failed", "cancelled"} {
		r := controlResult("prepare")
		r.Operation.State, r.Preview = state, nil
		if _, err := DecodeFrame(encodeFrame(t, "config.result", r)); err != nil {
			t.Fatalf("prepare replay %s rejected: %v", state, err)
		}
	}
	r := controlResult("inspect")
	r.Inventory.State = "read_only"
	r.Inventory.Objects[0].Writable = false
	r.Inventory.Objects = append(r.Inventory.Objects, r.Inventory.Objects[0])
	r.Inventory.Issues = []ConfigIssue{{Code: "duplicate_name"}}
	if err := r.Validate(); err != nil {
		t.Fatal("conflicting source names must remain observable", err)
	}
}

func TestConfigControlStrictWireAndSecretIsolation(t *testing.T) {
	command := string(encodeFrame(t, "config.command", controlCommand("prepare")))
	result := string(encodeFrame(t, "config.result", controlResult("inspect")))
	tests := []string{
		strings.Replace(command, `"request_id":`, `"request_id":"`+controlID(9)+`","request_id":`, 1),
		strings.Replace(command, `"request_id":`, `"requestId":`, 1),
		strings.Replace(command, `"action":"prepare"`, `"action":"prepare","raw_config":"synthetic-secret"`, 1),
		strings.Replace(command, `"value":8000`, `"value":8000,"password":"synthetic-secret"`, 1),
		strings.Replace(command, `"value":8000`, `"value":{"localPort":80}`, 1),
		strings.Replace(command, `,"value":8000`, "", 1),
		strings.Replace(command, `"jsonrpc":"2.0"`, `"jsonrpc":"2.0","id":"not-hello"`, 1),
		strings.Replace(result, `"present":true`, `"present":true,"value":"synthetic-secret"`, 1),
		strings.Replace(result, `"code":"ok"`, `"code":"ok","secret":{"value":"synthetic-secret"}`, 1),
		strings.Replace(result, `"source":"store"`, `"source":"store","native_path":"/private/config"`, 1),
		strings.Replace(result, `"writable":true,`, "", 1),
		strings.Replace(result, `"dependencies":[`, `"dependencies":null,"unused":[`, 1),
		`{"jsonrpc":"2.0","method":"config.command","params":null}`,
		`{"jsonrpc":"2.0","id":null,"method":"hello","params":null}`,
		`{"jsonrpc":"2.0","method":"report","params":null}`,
		strings.Replace(command, `"sequence":2`, `"sequence":null`, 1),
		strings.Replace(command, `"changes":[`, `"changes":null,"unused":[`, 1),
	}
	for i, data := range tests {
		if _, err := DecodeFrame([]byte(data)); err == nil {
			t.Fatalf("malformed wire %d accepted", i)
		}
	}
	// The one plaintext upload is accepted; none of the result shapes has a
	// credential-value field and safe projections preserve only presence.
	secret := controlCommand("secret")
	if _, err := DecodeFrame(encodeFrame(t, "config.command", secret)); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"inspect", "prepare", "apply", "query", "rollback", "cancel", "secret"} {
		data := encodeFrame(t, "config.result", controlResult(action))
		if strings.Contains(string(data), secret.Secret.Value) {
			t.Fatal("secret leaked into result")
		}
	}
}

func TestConfigControlSizeAndRequiredArrays(t *testing.T) {
	r := controlResult("inspect")
	r.Inventory.Objects = []ConfigObjectView{}
	for n := 0; n < MaxConfigObjects; n++ {
		o := controlObject()
		o.Name = fmt.Sprintf("proxy-%04d-%s", n, strings.Repeat("x", 128))
		r.Inventory.Objects = append(r.Inventory.Objects, o)
	}
	if r.Validate() == nil {
		t.Fatal("oversize safe inventory accepted")
	}
	if _, err := DecodeFrame(encodeFrame(t, "config.result", r)); err == nil {
		t.Fatal("oversize result frame accepted")
	}
	for _, mutate := range []func(*ConfigResult){
		func(r *ConfigResult) { r.Inventory.Objects = nil }, func(r *ConfigResult) { r.Inventory.Dependencies = nil }, func(r *ConfigResult) { r.Inventory.Issues = nil },
		func(r *ConfigResult) { r.Inventory.Objects[0].Fields = nil }, func(r *ConfigResult) { r.Inventory.Objects[0].Secrets = nil }, func(r *ConfigResult) { r.Inventory.Objects[0].ReadOnlyFields = nil }, func(r *ConfigResult) { r.Inventory.Objects[0].Issues = nil },
	} {
		r := controlResult("inspect")
		mutate(&r)
		if r.Validate() == nil {
			t.Fatal("missing required array accepted")
		}
	}
}

func TestConfigControlRawNullDoesNotRelaxLegacyFrames(t *testing.T) {
	for _, name := range []string{"hello", "report-first"} {
		for _, field := range []string{"schema", "session_id", "sequence", "collected_at"} {
			var frame map[string]json.RawMessage
			if err := json.Unmarshal(fixture(t, name), &frame); err != nil {
				t.Fatal(err)
			}
			var params map[string]json.RawMessage
			if err := json.Unmarshal(frame["params"], &params); err != nil {
				t.Fatal(err)
			}
			params[field] = json.RawMessage("null")
			frame["params"], _ = json.Marshal(params)
			data, _ := json.Marshal(frame)
			if _, err := DecodeFrame(data); err == nil {
				t.Fatalf("legacy %s admitted null %s", name, field)
			}
		}
	}
	var hello map[string]json.RawMessage
	_ = json.Unmarshal(fixture(t, "hello"), &hello)
	var params map[string]json.RawMessage
	_ = json.Unmarshal(hello["params"], &params)
	params["facts"] = json.RawMessage("null")
	hello["params"], _ = json.Marshal(params)
	data, _ := json.Marshal(hello)
	if _, err := DecodeFrame(data); err == nil {
		t.Fatal("legacy required object admitted null")
	}
	report := fixtureFrame(t, "report-first").Report
	report.Metrics.CPU.Quality = QualityOK
	report.Metrics.CPU.Value = nil
	if _, err := DecodeFrame(encodeFrame(t, "report", report)); err == nil {
		t.Fatal("legacy ok observation admitted null")
	}
}

func TestConfigCloneWireIsAnIdentityInstructionOnly(t *testing.T) {
	c := controlCommand("prepare")
	c.Changes[0].CloneFrom = "original"
	if _, err := DecodeFrame(encodeFrame(t, "config.command", c)); err != nil {
		t.Fatal("clone command rejected", err)
	}
	for _, mutate := range []func(*ConfigChange){
		func(c *ConfigChange) { c.CloneFrom = c.Name },
		func(c *ConfigChange) { c.CloneFrom = "bad\nname" },
		func(c *ConfigChange) { c.CloneFrom = strings.Repeat("x", 257) },
		func(c *ConfigChange) { c.Operation = "update"; c.Type = "" },
		func(c *ConfigChange) { c.Operation = "delete"; c.Type = ""; c.Fields = []ConfigFieldPatch{} },
	} {
		bad := controlCommand("prepare")
		bad.Changes[0].CloneFrom = "original"
		mutate(&bad.Changes[0])
		if _, err := DecodeFrame(encodeFrame(t, "config.command", bad)); err == nil {
			t.Fatal("invalid clone request accepted")
		}
	}
	raw := string(encodeFrame(t, "config.command", c))
	for _, bad := range []string{
		strings.Replace(raw, `"clone_from":"original"`, `"cloneFrom":"original"`, 1),
		strings.Replace(raw, `"clone_from":"original"`, `"clone_from":{"name":"original","password":"synthetic-secret"}`, 1),
		strings.Replace(raw, `"clone_from":"original"`, `"clone_from":"original","native_config":{"password":"synthetic-secret"}`, 1),
	} {
		if _, err := DecodeFrame([]byte(bad)); err == nil {
			t.Fatal("clone request carried raw native material")
		}
	}
}
