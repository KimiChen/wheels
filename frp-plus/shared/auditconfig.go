package shared

import "errors"

// AuditConfig governs administrative audit retention, independently of TSDB.
// Limits reject new configuration work; recovery evidence is never evicted.
type AuditConfig struct {
	RetentionDays int   `json:"retentionDays,omitempty"`
	MaxRows       int64 `json:"maxRows,omitempty"`
	MaxBytes      int64 `json:"maxBytes,omitempty"`
}

func (c *AuditConfig) Complete() {
	if c.RetentionDays == 0 {
		c.RetentionDays = 90
	}
	if c.MaxRows == 0 {
		c.MaxRows = 100000
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 256 << 20
	}
}

func (c AuditConfig) Validate() error {
	c.Complete()
	if c.RetentionDays < 1 || c.RetentionDays > 366 || c.MaxRows < 100 || c.MaxRows > 10000000 || c.MaxBytes < 1<<20 || c.MaxBytes > 16<<30 {
		return errors.New("invalid monitor audit retention or capacity")
	}
	return nil
}
