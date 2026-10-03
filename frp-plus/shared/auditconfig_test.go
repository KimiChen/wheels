package shared

import "testing"

func TestAuditConfigurationDefaultsAndBounds(t *testing.T) {
	c := AuditConfig{}
	c.Complete()
	if c.RetentionDays != 90 || c.MaxRows != 100000 || c.MaxBytes != 256<<20 || c.Validate() != nil {
		t.Fatal(c)
	}
	for _, bad := range []AuditConfig{{RetentionDays: -1}, {RetentionDays: 367}, {MaxRows: 99}, {MaxRows: 10000001}, {MaxBytes: (1 << 20) - 1}, {MaxBytes: (16 << 30) + 1}} {
		if bad.Validate() == nil {
			t.Fatal("accepted", bad)
		}
	}
}
