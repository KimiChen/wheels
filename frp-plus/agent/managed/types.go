// Package managed owns one fixed local Store file and its durable transaction
// journal. It knows no FRP types, network protocol, telemetry or remote identity.
// A native adapter must validate complete candidates and source ownership before
// Prepare, and supply whole-set Apply plus independently observed Verify hooks.
package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

const (
	Prepared       = "prepared"
	Applying       = "applying"
	Verifying      = "verifying"
	Confirmed      = "confirmed"
	OutcomeUnknown = "outcome_unknown"
	RollingBack    = "rolling_back"
	RolledBack     = "rolled_back"
	RollbackFailed = "rollback_failed"
	Conflict       = "conflict"
	Cancelled      = "cancelled"
)

var (
	ErrInvalid        = errors.New("invalid managed Store request")
	ErrUnsafePath     = errors.New("unsafe managed Store path or permissions")
	ErrLocked         = errors.New("managed Store is already locked")
	ErrConflict       = errors.New("managed Store revision conflict")
	ErrBusy           = errors.New("managed Store transaction is active")
	ErrNotFound       = errors.New("managed Store operation not found")
	ErrExpired        = errors.New("managed Store operation deadline expired")
	ErrStorage        = errors.New("managed Store persistence failed")
	ErrRecovery       = errors.New("managed Store recovery requires local repair")
	ErrRuntime        = errors.New("managed Store runtime is unavailable")
	ErrOutcomeUnknown = errors.New("managed Store runtime outcome remains unknown")
	ErrClosed         = errors.New("managed Store engine is closed")
)

type Options struct {
	// Root is an absolute private directory; StorePath is its fixed direct child.
	// No request can select a path. Existing permissions are checked, not fixed.
	Root            string
	StorePath       string
	MaxBytes        int64
	MaxOperations   int
	RollbackTimeout time.Duration
	CloseTimeout    time.Duration
	// RecoveryCheck validates the saved non-Store context before an interrupted
	// operation can restore disk, and therefore before the native Store loader.
	// It is required when recovery material contains an unfinished operation.
	RecoveryCheck func(context.Context, Operation) error
}

// StoreSnapshot distinguishes a missing initial Store from an existing empty
// file. Bytes are private and must never be included in protocol responses.
type StoreSnapshot struct {
	Exists bool
	Bytes  []byte
}

// Digest includes file presence, so deleting a zero-byte Store is still drift.
func Digest(s StoreSnapshot) string {
	h := sha256.New()
	if s.Exists {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(s.Bytes)
	return hex.EncodeToString(h.Sum(nil))
}

type Request struct {
	ID              string
	IdempotencyKey  string
	BaseRevision    string
	ContextRevision string
	RequestDigest   string
	ExpectedDigest  string
	Candidate       []byte
	Deadline        time.Time
}

// Verification describes actual local observation, not merely a nil Apply
// error. ResourcesReady means the native adapter checked the expected runtime
// resources. It must not imply business reachability when BusinessChecked=false.
type Verification struct {
	RuntimeLoaded   bool `json:"runtime_loaded"`
	ResourcesReady  bool `json:"resources_ready"`
	BusinessChecked bool `json:"business_checked"`
}

type Runtime struct {
	// Check is a read-only native source CAS. apply checks the complete base
	// plus context revision; rollback/recovery check context excluding Store.
	Check func(context.Context, Operation, string) error
	// Apply replaces the complete in-memory Store and reloads the complete set
	// once. It must not rewrite the disk Store owned by this package. Hooks must
	// honor context cancellation; an uncooperative hook keeps the engine locked.
	Apply  func(context.Context, StoreSnapshot) error
	Verify func(context.Context, StoreSnapshot) (Verification, error)
}

// Operation is safe metadata. It never contains candidate bytes, paths, native
// error strings, credentials, or snapshots. ErrorCode is a fixed local code.
type Operation struct {
	ID              string       `json:"id"`
	BaseRevision    string       `json:"base_revision"`
	ContextRevision string       `json:"context_revision"`
	RequestDigest   string       `json:"request_digest"`
	OldDigest       string       `json:"old_digest"`
	NewDigest       string       `json:"new_digest"`
	State           string       `json:"state"`
	ErrorCode       string       `json:"error_code,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	Deadline        time.Time    `json:"deadline"`
	StorePersisted  bool         `json:"store_persisted"`
	RuntimeApplied  bool         `json:"runtime_applied"`
	Verification    Verification `json:"verification"`
}

func cloneSnapshot(s StoreSnapshot) StoreSnapshot {
	return StoreSnapshot{Exists: s.Exists, Bytes: append([]byte(nil), s.Bytes...)}
}

func terminal(state string) bool {
	switch state {
	case Confirmed, RolledBack, Conflict, Cancelled:
		return true
	}
	return false
}
