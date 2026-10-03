package shared

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigIdentityAndDigest(t *testing.T) {
	seen := make(map[string]bool)
	for range 256 {
		id, err := NewConfigOperationID()
		if err != nil || !ValidConfigOperationID(id) || seen[id] {
			t.Fatalf("invalid or repeated operation identity: %v", err)
		}
		seen[id] = true
	}
	for _, id := range []string{"", "01234567-89ab-4cde-0123-456789abcdef", "01234567-89ab-1cde-8123-456789abcdef", "01234567-89AB-4cde-8123-456789abcdef", "../operations"} {
		if ValidConfigOperationID(id) {
			t.Fatalf("accepted invalid operation identity %q", id)
		}
	}
	if !ValidConfigDigest(strings.Repeat("ab", 32)) {
		t.Fatal("rejected SHA-256 digest")
	}
	for _, digest := range []string{"", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if ValidConfigDigest(digest) {
			t.Fatal("accepted invalid digest")
		}
	}
}

func TestConfigOperationRecoveryRetainsLease(t *testing.T) {
	for _, state := range []string{"draft", "validated", "prepared", "applying", "verifying", "outcome_unknown", "rolling_back", "rollback_failed"} {
		if !ConfigOperationActive(state) || !ValidConfigOperationState(state) {
			t.Fatalf("state must retain lease: %s", state)
		}
	}
	for _, state := range []string{"confirmed", "rejected", "conflict", "failed", "cancelled", "rolled_back", "invalid"} {
		if ConfigOperationActive(state) {
			t.Fatalf("unexpected active lease: %s", state)
		}
	}
	for _, path := range [][]string{
		{"draft", "validated", "prepared", "applying", "verifying", "confirmed", "rolling_back", "rolled_back"},
		{"prepared", "outcome_unknown", "verifying", "rolling_back", "rollback_failed", "rolling_back", "rolled_back"},
		{"applying", "outcome_unknown", "confirmed"},
	} {
		for i := 1; i < len(path); i++ {
			if !ConfigOperationTransitionAllowed(path[i-1], path[i]) {
				t.Fatalf("recovery transition rejected: %s -> %s", path[i-1], path[i])
			}
		}
	}
	for _, pair := range [][2]string{{"draft", "confirmed"}, {"prepared", "confirmed"}, {"verifying", "failed"}, {"applying", "cancelled"}, {"rolled_back", "applying"}, {"failed", "prepared"}, {"rollback_failed", "cancelled"}, {"invalid", "draft"}, {"prepared", "prepared"}} {
		if ConfigOperationTransitionAllowed(pair[0], pair[1]) {
			t.Fatalf("unsafe transition accepted: %s -> %s", pair[0], pair[1])
		}
	}
	if !ValidConfigEventCode("created") || ValidConfigEventCode("native error with a password") {
		t.Fatal("audit reason must use closed vocabulary")
	}
}

func TestConfigPatchesCannotCarryNativeCredentialObjects(t *testing.T) {
	for _, field := range []ConfigFieldPatch{
		{Path: "localPort", Value: json.RawMessage(`8080`)},
		{Path: "enabled", Value: json.RawMessage(`false`)},
		{Path: "customDomains", Value: json.RawMessage(`["service.example.test"]`)},
		{Path: "subdomain", Value: json.RawMessage(`null`)},
	} {
		if err := field.Validate(); err != nil {
			t.Fatalf("rejected supported value: %s: %v", field.Path, err)
		}
	}
	for _, field := range []ConfigFieldPatch{
		{Path: "secretKey", Value: json.RawMessage(`"credential"`)},
		{Path: "httpPassword", Value: json.RawMessage(`"credential"`)},
		{Path: "plugin", Value: json.RawMessage(`{"password":"credential"}`)},
		{Path: "localIP", Value: json.RawMessage(`{"password":"credential"}`)},
		{Path: "localPort", Value: json.RawMessage(`1.5`)},
		{Path: "localPort", Value: json.RawMessage(`9223372036854775808`)},
		{Path: "localPort", Value: json.RawMessage(`80 81`)},
		{Path: "customDomains", Value: json.RawMessage(`[["nested"]]`)},
		{Path: "customDomains", Value: json.RawMessage(`[null]`)},
		{Path: "localIP", Value: json.RawMessage(`"line\nbreak"`)},
	} {
		if field.Validate() == nil {
			t.Fatalf("accepted invalid patch: %s", field.Path)
		}
	}
	id, _ := NewConfigOperationID()
	for _, secret := range []ConfigSecretPatch{{Path: "secretKey", Mode: "keep"}, {Path: "httpPassword", Mode: "clear"}, {Path: "loadBalancer.groupKey", Mode: "reference", Reference: id}} {
		if err := secret.Validate(); err != nil {
			t.Fatalf("invalid local secret instruction: %v", err)
		}
	}
	for _, secret := range []ConfigSecretPatch{{Path: "secretKey", Mode: "replace", Reference: "credential"}, {Path: "secretKey", Mode: "keep", Reference: id}, {Path: "secretKey", Mode: "reference", Reference: "../key"}, {Path: "plugin.password", Mode: "clear"}} {
		if secret.Validate() == nil {
			t.Fatal("accepted unsafe secret instruction")
		}
	}
}

func TestConfigChangeBoundsAndObjectIdentity(t *testing.T) {
	change := ConfigChange{Operation: "create", Kind: "proxy", Name: "sample", Type: "tcp", Fields: []ConfigFieldPatch{{Path: "localPort", Value: json.RawMessage(`8080`)}}, Secrets: []ConfigSecretPatch{}}
	if err := ValidateConfigChanges([]ConfigChange{change}); err != nil {
		t.Fatal(err)
	}
	visitor := ConfigChange{Operation: "create", Kind: "visitor", Name: change.Name, Type: "stcp", Fields: []ConfigFieldPatch{}, Secrets: []ConfigSecretPatch{}}
	if err := ValidateConfigChanges([]ConfigChange{change, visitor}); err != nil {
		t.Fatal("proxy and visitor have distinct identities", err)
	}
	if ValidateConfigChanges([]ConfigChange{change, change}) == nil || ValidateConfigChanges(nil) == nil {
		t.Fatal("duplicate or empty changes accepted")
	}
	duplicate := change
	duplicate.Fields = append(append([]ConfigFieldPatch{}, change.Fields...), change.Fields[0])
	if duplicate.Validate() == nil {
		t.Fatal("duplicate field accepted")
	}
	for _, operation := range []string{"enable", "disable", "delete"} {
		bad := change
		bad.Operation, bad.Type = operation, ""
		if bad.Validate() == nil {
			t.Fatal("non-edit action accepted extra fields")
		}
	}
	bad := visitor
	bad.Type = "tcp"
	if bad.Validate() == nil {
		t.Fatal("unsupported visitor type accepted")
	}
	bad = change
	bad.Operation = "update"
	if bad.Validate() == nil {
		t.Fatal("type replacement must be delete and create")
	}
	large := make([]ConfigChange, MaxConfigChanges+1)
	if ValidateConfigChanges(large) == nil {
		t.Fatal("excessive changes accepted")
	}
	large = nil
	for range 256 {
		id, _ := NewConfigOperationID()
		item := change
		item.Name = id
		item.Fields = []ConfigFieldPatch{{Path: "localIP", Value: json.RawMessage(`"` + strings.Repeat("a", 1000) + `"`)}}
		large = append(large, item)
	}
	if ValidateConfigChanges(large) == nil {
		t.Fatal("aggregate payload limit not enforced")
	}
}
