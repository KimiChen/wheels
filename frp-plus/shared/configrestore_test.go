package shared

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const restoreTestID = "00000000-0000-4000-8000-000000000001"

func restoreCommandFixture() RestoreCommand {
	now := time.Now().UTC()
	return RestoreCommand{Meta: Meta{Schema: 1, SessionID: "session", Sequence: 2, CollectedAt: now.Format(time.RFC3339Nano)}, RequestID: restoreTestID, ServiceID: restoreTestID, Action: "acknowledge", Epoch: restoreTestID, ManifestDigest: strings.Repeat("a", 64), ContextRevision: strings.Repeat("b", 64), StoreDigest: strings.Repeat("c", 64), AcknowledgementID: restoreTestID, DeadlineAtMS: now.Add(time.Second).UnixMilli()}
}
func restoreInfoFixture(state string) RestoreInfo {
	c := restoreCommandFixture()
	i := RestoreInfo{State: state, Epoch: c.Epoch, BackupServiceID: restoreTestID, ManifestDigest: c.ManifestDigest, ContextRevision: c.ContextRevision, OperationsCount: 4}
	if state != "pending" {
		i.StoreDigest = c.StoreDigest
		i.RuntimeLoaded = true
		i.ResourcesReady = true
	}
	if state == "acknowledged" || state == "confirmed" {
		i.AcknowledgementID = c.AcknowledgementID
	}
	return i
}
func TestRestoreInfoStateFacts(t *testing.T) {
	for _, state := range []string{"pending", "verified", "acknowledged", "confirmed"} {
		if err := restoreInfoFixture(state).Validate(); err != nil {
			t.Fatal(state, err)
		}
	}
	if err := (RestoreInfo{State: "none"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*RestoreInfo){
		"none-data":           func(i *RestoreInfo) { i.State = "none" },
		"pending-false-facts": func(i *RestoreInfo) { i.State = "pending" },
		"missing-loaded":      func(i *RestoreInfo) { i.RuntimeLoaded = false },
		"missing-ready":       func(i *RestoreInfo) { i.ResourcesReady = false },
		"unverified-ack":      func(i *RestoreInfo) { i.State = "verified" },
		"missing-ack":         func(i *RestoreInfo) { i.AcknowledgementID = "" },
		"oversize-history":    func(i *RestoreInfo) { i.OperationsCount = 1025 },
		"negative-history":    func(i *RestoreInfo) { i.OperationsCount = -1 },
		"path-in-id":          func(i *RestoreInfo) { i.ReplacedServiceID = "/private/path" },
		"unknown-state":       func(i *RestoreInfo) { i.State = "ready" },
	} {
		t.Run(name, func(t *testing.T) {
			i := restoreInfoFixture("acknowledged")
			edit(&i)
			if i.Validate() == nil {
				t.Fatal("accepted invalid facts")
			}
		})
	}
}
func TestRestoreWireStrictAndSeparate(t *testing.T) {
	c := restoreCommandFixture()
	if err := c.ValidateAt(time.Now()); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "config.restore.command", "params": c})
	f, err := DecodeFrame(data)
	if err != nil || f.RestoreCommand == nil || f.ConfigCommand != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(string) string{
		"legacy-method":   func(s string) string { return strings.Replace(s, "config.restore.command", "config.command", 1) },
		"unknown-field":   func(s string) string { return strings.Replace(s, `"action":`, `"path":"/private/path","action":`, 1) },
		"duplicate-field": func(s string) string { return strings.Replace(s, `"action":`, `"action":"inspect","action":`, 1) },
		"missing-field":   func(s string) string { return strings.Replace(s, `"epoch":"`+restoreTestID+`",`, "", 1) },
		"oversize-frame":  func(s string) string { return strings.Repeat(" ", MaxRestoreControlBytes) + s },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeFrame([]byte(mutate(string(data)))); err == nil {
				t.Fatal("accepted invalid frame")
			}
		})
	}
	c.DeadlineAtMS = time.Now().Add(-time.Second).UnixMilli()
	if c.ValidateAt(time.Now()) == nil {
		t.Fatal("expired admitted")
	}
	c = restoreCommandFixture()
	c.DeadlineAtMS = time.Now().Add(61 * time.Second).UnixMilli()
	if c.ValidateAt(time.Now()) == nil {
		t.Fatal("unbounded deadline")
	}
	c = restoreCommandFixture()
	c.Action = "inspect"
	if c.Validate() == nil {
		t.Fatal("inspect carried mutation identity")
	}
}
func TestRestoreResultBindsOnlySuccessfulAcknowledgement(t *testing.T) {
	c := restoreCommandFixture()
	i := restoreInfoFixture("acknowledged")
	r := RestoreResult{Meta: c.Meta, RequestID: c.RequestID, ServiceID: c.ServiceID, Action: c.Action, Code: "ok", Restore: &i}
	if !MatchesRestoreResult(c, r) {
		t.Fatal("valid result rejected")
	}
	i.Epoch = "00000000-0000-4000-8000-000000000002"
	if MatchesRestoreResult(c, r) {
		t.Fatal("wrong restore acknowledgement accepted")
	}
	r.Code = "conflict"
	if !MatchesRestoreResult(c, r) {
		t.Fatal("current conflicting facts lost")
	}
	r.Code = "ok"
	r.Restore = nil
	if r.Validate() == nil {
		t.Fatal("ok without facts")
	}
	r.Code = "internal_error"
	if r.Validate() != nil {
		t.Fatal("fixed error without facts rejected")
	}
}

func TestRestoreSupersededLocalAuditCode(t *testing.T) {
	if !ValidConfigEventCode("restore_superseded") {
		t.Fatal("missing fixed local audit code")
	}
	if ValidConfigEventCode("restore_superseded: private reason") {
		t.Fatal("free-form audit code accepted")
	}
}
