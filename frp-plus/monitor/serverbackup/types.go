// Package serverbackup captures and transforms private, explicitly authorized
// frps context. Database and TSDB snapshots are owned by the offline archive tool.
package serverbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const MaxFileBytes = 1 << 20
const MaxFiles = 128
const MaxTotalBytes = 16 << 20

type PathGrant struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}
type Policy struct {
	Version    int         `json:"version"`
	ConfigFile string      `json:"config_file"`
	WorkingDir string      `json:"working_dir"`
	Roots      []PathGrant `json:"roots"`
	Files      []PathGrant `json:"files"`
}
type File struct {
	Path    string `json:"path"`
	Payload string `json:"payload"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	MtimeNS int64  `json:"mtime_ns"`
}
type TemplateRequirement struct {
	From           string   `json:"from"`
	Names          []string `json:"names"`
	RenderedSHA256 string   `json:"rendered_sha256"`
}
type Manifest struct {
	Kind                 string                `json:"kind"`
	Version              int                   `json:"version"`
	ConfigFile           string                `json:"config_file"`
	CWD                  string                `json:"cwd"`
	PolicyDigest         string                `json:"policy_digest"`
	ContextRevision      string                `json:"context_revision"`
	DatabaseFile         string                `json:"database_file"`
	HistoryPath          string                `json:"history_path"`
	HistoryEnabled       bool                  `json:"history_enabled"`
	LogPath              string                `json:"log_path"`
	TemplateRequirements []TemplateRequirement `json:"template_requirements"`
	Files                []File                `json:"files"`
}
type Summary struct {
	Code            string `json:"code"`
	Version         int    `json:"version"`
	ManifestDigest  string `json:"manifest_digest"`
	ContextRevision string `json:"context_revision"`
	FileCount       int    `json:"file_count"`
	TotalBytes      int64  `json:"total_bytes"`
}
type material struct {
	data  []byte
	mtime int64
}
type Snapshot struct {
	Manifest Manifest
	Policy   Policy
	payload  map[string]material
}

func failure(code string) error { return errors.New("server_backup_" + code) }
func hash(data []byte) string   { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil && s == stringLower(s)
}
