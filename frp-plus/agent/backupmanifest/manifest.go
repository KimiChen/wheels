// Package backupmanifest defines private, versioned dependency inventories.
// It contains no filesystem access and is not a complete managed checkpoint.
package backupmanifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const (
	Version          = 2
	Kind             = "frp-agent-dependency-graph"
	MaxManifestBytes = 1 << 20
	MaxFiles         = 128
	MaxDirectories   = 128
	MaxEntries       = 8192
	MaxEdges         = 8192
	MaxFileBytes     = 1 << 20
	MaxTotalBytes    = 4 << 20
)

var ErrInvalid = errors.New("backup_manifest_invalid")

// Manifest and its per-file digests are PRIVATE. Public summaries must expose
// only the overall digest and counts, never filenames, native fields or bytes.
type Manifest struct {
	Version      int         `json:"version"`
	Kind         string      `json:"kind"`
	PolicyDigest string      `json:"policy_digest"`
	ConfigFile   string      `json:"config_file"`
	WorkingDir   string      `json:"working_dir"`
	StoreFile    string      `json:"store_file"`
	Files        []File      `json:"files"`
	Directories  []Directory `json:"directories"`
	Includes     []Include   `json:"includes"`
	References   []Reference `json:"references"`
}

type File struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	LogicalPath string `json:"logical_path"`
	Kind        string `json:"kind"`
	Exists      bool   `json:"exists"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Mode        uint32 `json:"mode"`
	ModifiedNS  int64  `json:"modified_ns"`
}

// Entries are the full bounded directory inventory, including unmatched
// names. Replaying includes never consults the current machine's directory.
type Directory struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
}
type Entry struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // file, directory, other
}
type Include struct {
	Pattern string   `json:"pattern"`
	Files   []string `json:"files"`
}
type Reference struct {
	From  string `json:"from"`
	Field string `json:"field"`
	To    string `json:"to"`
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func ValidDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func Encode(value Manifest) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxManifestBytes {
		return nil, ErrInvalid
	}
	if _, err := Decode(data); err != nil {
		return nil, err
	}
	return data, nil
}

// Decode bounds allocation before decoding, rejects duplicate/unknown keys,
// and leaves semantic closure and caller-owned path policy to configuration.
func Decode(data []byte) (Manifest, error) {
	var out Manifest
	if len(data) == 0 || len(data) > MaxManifestBytes || !utf8.Valid(data) {
		return out, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if scan(d, 0) != nil {
		return out, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return out, ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || out.Version != Version || out.Kind != Kind || !ValidDigest(out.PolicyDigest) ||
		len(out.Files) == 0 || len(out.Files) > MaxFiles || len(out.Directories) > MaxDirectories ||
		len(out.Includes) > MaxDirectories || len(out.References) > MaxEdges || out.Files == nil || out.Directories == nil || out.Includes == nil || out.References == nil {
		return Manifest{}, ErrInvalid
	}
	entries := 0
	for _, dir := range out.Directories {
		entries += len(dir.Entries)
		if dir.Entries == nil || entries > MaxEntries {
			return Manifest{}, ErrInvalid
		}
	}
	return out, nil
}

func scan(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalid
	}
	t, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || !canonicalKey(name) || seen[name] {
				return ErrInvalid
			}
			seen[name] = true
			if err := scan(d, depth+1); err != nil {
				return err
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for d.More() {
			if err := scan(d, depth+1); err != nil {
				return err
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// encoding/json matches struct keys with Unicode case folding. The scanner
// must require exact spellings before that decoder can erase aliases. Object
// placement is then checked by DisallowUnknownFields.
func canonicalKey(key string) bool {
	switch key {
	case "version", "kind", "policy_digest", "config_file", "working_dir", "store_file", "files", "directories", "includes", "references",
		"id", "path", "logical_path", "exists", "size", "sha256", "mode", "modified_ns", "entries", "name", "pattern", "from", "field", "to":
		return true
	default:
		return false
	}
}
