package configuration

import (
	"context"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
)

type backupSealedView struct {
	files                      map[string]backupSourceFile
	directories                map[string][]backupmanifest.Entry
	usedFiles, usedDirectories map[string]bool
}

func backupEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && utf8.ValidString(name) && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00\r\n")
}
func backupFileMode(mode uint32) bool {
	return mode & ^uint32(0777) == 0 && mode&0022 == 0 && mode&0111 == 0 && mode&0400 != 0
}

func newBackupSealedView(p BackupGraphPolicy, m backupmanifest.Manifest, payloads []BackupGraphFile) (*backupSealedView, error) {
	v := &backupSealedView{files: map[string]backupSourceFile{}, directories: map[string][]backupmanifest.Entry{}, usedFiles: map[string]bool{}, usedDirectories: map[string]bool{}}
	if len(m.Files) > p.Limits.Files || len(m.Directories) > p.Limits.Directories || len(m.Includes) > p.Limits.Directories || len(m.References) > p.Limits.Edges || len(payloads) > p.Limits.Files {
		return nil, backupErr("limit_exceeded")
	}
	data := map[string][]byte{}
	var total int64
	for _, payload := range payloads {
		if _, exists := data[payload.ID]; exists || len(payload.ID) != 7 {
			return nil, backupErr("manifest_invalid")
		}
		total += int64(len(payload.Bytes))
		if int64(len(payload.Bytes)) > p.Limits.FileBytes || total > p.Limits.TotalBytes {
			return nil, backupErr("limit_exceeded")
		}
		data[payload.ID] = append([]byte(nil), payload.Bytes...)
	}
	ids := map[string]bool{}
	for _, entry := range m.Files {
		logical, err := p.logical(entry.Path, false)
		if err != nil || logical != entry.LogicalPath || ids[entry.ID] {
			return nil, backupErr("policy_mismatch")
		}
		ids[entry.ID] = true
		if _, exists := v.files[entry.Path]; exists {
			return nil, backupErr("manifest_invalid")
		}
		if entry.Kind != "main" && entry.Kind != "include" && entry.Kind != "store" && entry.Kind != "local_file" {
			return nil, backupErr("manifest_invalid")
		}
		payload, exists := data[entry.ID]
		if entry.Exists {
			if !exists || entry.Size != int64(len(payload)) || entry.Size > p.Limits.FileBytes || !backupFileMode(entry.Mode) || entry.ModifiedNS <= 0 || !backupmanifest.ValidDigest(entry.SHA256) || backupmanifest.Digest(payload) != entry.SHA256 {
				return nil, backupErr("payload_invalid")
			}
			delete(data, entry.ID)
		} else if exists || entry.Kind != "store" || entry.Size != 0 || entry.SHA256 != "" || entry.Mode != 0 || entry.ModifiedNS != 0 {
			return nil, backupErr("payload_invalid")
		}
		v.files[entry.Path] = backupSourceFile{data: payload, exists: entry.Exists, mode: entry.Mode, modified: entry.ModifiedNS}
	}
	if len(data) != 0 {
		return nil, backupErr("payload_invalid")
	}
	count := 0
	for _, dir := range m.Directories {
		if _, err := p.logical(dir.Path, true); err != nil {
			return nil, err
		}
		if _, exists := v.directories[dir.Path]; exists {
			return nil, backupErr("manifest_invalid")
		}
		count += len(dir.Entries)
		if count > p.Limits.Entries {
			return nil, backupErr("limit_exceeded")
		}
		previous := ""
		for _, entry := range dir.Entries {
			if !backupEntryName(entry.Name) || entry.Name <= previous || entry.Kind != "file" && entry.Kind != "directory" && entry.Kind != "other" {
				return nil, backupErr("manifest_invalid")
			}
			previous = entry.Name
		}
		v.directories[dir.Path] = append([]backupmanifest.Entry{}, dir.Entries...)
	}
	return v, nil
}
func (v *backupSealedView) read(ctx context.Context, path string) (backupSourceFile, error) {
	if err := ctx.Err(); err != nil {
		return backupSourceFile{}, err
	}
	file, exists := v.files[path]
	if !exists {
		return backupSourceFile{}, backupErr("dependency_missing")
	}
	v.usedFiles[path] = true
	file.data = append([]byte(nil), file.data...)
	return file, nil
}
func (v *backupSealedView) directory(ctx context.Context, path string) ([]backupmanifest.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, exists := v.directories[path]
	if !exists {
		return nil, backupErr("inventory_missing")
	}
	v.usedDirectories[path] = true
	return append([]backupmanifest.Entry{}, entries...), nil
}
