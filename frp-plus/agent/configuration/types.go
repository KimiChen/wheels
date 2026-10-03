// Package configuration inspects native FRP inputs and prepares Store-only
// candidates. It performs no writes, native reloads, network calls or commands.
package configuration

import (
	"encoding/json"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

const (
	MaxInputBytes   = 4 << 20
	MaxObjects      = 1024
	MaxDependencies = 128
	MaxChanges      = 512
)

// Input is private process state. The caller must clone it under the native
// configuration locks and provide ALL source objects, including disabled ones.
// StoreFile has already passed the owner's managed-root path policy.
type Input struct {
	ConfigFile      string
	StoreFile       string
	WorkingDir      string
	StartupCommon   *v1.ClientCommonConfig
	ReloadCommon    *v1.ClientCommonConfig
	FileMemory      Objects
	StoreMemory     Objects
	TemplateEnv     map[string]string
	HTTPProxy       string
	UnsafeFeatures  []string
	VirtualNetReady bool
	// Values are supplied by a local secret vault, never by wire DTOs or paths.
	LocalSecrets map[string]string
}

type Objects struct {
	Proxies  []v1.ProxyConfigurer
	Visitors []v1.VisitorConfigurer
}

type Issue struct {
	Code string `json:"code"`
}
type SecretState struct {
	Present bool `json:"present"`
}
type DependencyView struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
}

// ObjectView is an allowlisted preview, never a serialization of native config.
type ObjectView struct {
	Kind           string                     `json:"kind"`
	Name           string                     `json:"name"`
	Type           string                     `json:"type"`
	Source         string                     `json:"source"`
	Writable       bool                       `json:"writable"`
	Active         bool                       `json:"active"`
	Fields         map[string]json.RawMessage `json:"fields"`
	Secrets        map[string]SecretState     `json:"secrets"`
	ReadOnlyFields []string                   `json:"read_only_fields"`
	Issues         []Issue                    `json:"issues"`
}

type Inspection struct {
	Revision        string           `json:"revision"`
	ContextRevision string           `json:"context_revision"`
	State           string           `json:"state"`
	Objects         []ObjectView     `json:"objects"`
	Dependencies    []DependencyView `json:"dependencies"`
	Issues          []Issue          `json:"issues"`
}

type SecretAction struct{ Mode, Reference string }
type Change struct {
	Operation string
	Kind      string
	Name      string
	Type      string
	CloneFrom string
	Fields    map[string]json.RawMessage
	Secrets   map[string]SecretAction
}
type Request struct {
	ExpectedRevision string
	Changes          []Change
}

type ChangePreview struct {
	Operation string      `json:"operation"`
	Before    *ObjectView `json:"before"`
	After     *ObjectView `json:"after"`
}

type Prepared struct {
	BaseRevision    string          `json:"base_revision"`
	ContextRevision string          `json:"context_revision"`
	CandidateSHA256 string          `json:"candidate_sha256"`
	Changes         []ChangePreview `json:"changes"`
	Warnings        []Issue         `json:"warnings"`
	// Private transaction materials. The Engine must re-inspect/CAS under its
	// mutation lock before committing, and owns persistence/rollback/reload.
	StoreBytes          []byte  `json:"-"`
	OriginalStoreBytes  []byte  `json:"-"`
	OriginalStoreExists bool    `json:"-"`
	Store               Objects `json:"-"`
	Effective           Objects `json:"-"`
}

// Error contains stable codes only. Native errors can contain secrets/paths and
// are deliberately not wrapped or exposed in managed previews.
type Error struct{ Code string }

func (e *Error) Error() string  { return e.Code }
func failure(code string) error { return &Error{Code: code} }
