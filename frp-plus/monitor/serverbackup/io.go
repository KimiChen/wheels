package serverbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func openNoFollow(path string, dir bool) (*os.File, error) {
	if !absolute(path) {
		return nil, failure("path_invalid")
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, failure("read_failed")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, name := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if i < len(parts)-1 || dir {
			flags |= unix.O_DIRECTORY
		}
		next, e := unix.Openat(fd, name, flags, 0)
		unix.Close(fd)
		if e != nil {
			return nil, failure("read_failed")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func readPrivate(path string, strict bool) (material, error) {
	var out material
	f, e := openNoFollow(path, false)
	if e != nil {
		return out, e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return out, failure("read_failed")
	}
	sys, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || sys.Uid != uint32(os.Geteuid()) || sys.Nlink != 1 || before.Mode().Perm()&0022 != 0 || strict && before.Mode().Perm() != 0600 || before.Size() < 0 || before.Size() > MaxFileBytes {
		return out, failure("file_unsafe")
	}
	data, e := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	after, ae := f.Stat()
	named, ne := openNoFollow(path, false)
	if ne == nil {
		defer named.Close()
	}
	if e != nil || ae != nil || ne != nil {
		return out, failure("source_changed")
	}
	current, ce := named.Stat()
	if ce != nil || !os.SameFile(before, after) || !os.SameFile(before, current) || before.Size() != int64(len(data)) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.ModTime() != current.ModTime() || before.Mode() != current.Mode() {
		return out, failure("source_changed")
	}
	return material{data, before.ModTime().UnixNano()}, nil
}
func Capture(ctx context.Context, p Policy, env map[string]string, forbidden []string) (Snapshot, error) {
	p, e := normalize(p)
	if e != nil {
		return Snapshot{}, e
	}
	source := map[string]material{}
	read := func(path string) (material, error) {
		if v, ok := source[path]; ok {
			return v, nil
		}
		v, e := readPrivate(path, false)
		if e == nil {
			source[path] = v
		}
		return v, e
	}
	snapshot, e := build(ctx, p, env, read)
	if e != nil {
		return snapshot, e
	}
	if e = snapshot.protect(forbidden); e != nil {
		return Snapshot{}, e
	}
	for path, before := range source {
		if e = ctx.Err(); e != nil {
			return Snapshot{}, failure("cancelled")
		}
		after, e := readPrivate(path, false)
		if e != nil || before.mtime != after.mtime || !bytes.Equal(before.data, after.data) {
			return Snapshot{}, failure("source_changed")
		}
	}
	return snapshot, nil
}
func build(ctx context.Context, p Policy, env map[string]string, read func(string) (material, error)) (Snapshot, error) {
	out := Snapshot{Policy: p, payload: map[string]material{}}
	main, e := read(p.ConfigFile)
	if e != nil {
		return out, e
	}
	_, cfg, requirements, e := decodeConfig(ctx, p, main.data, env)
	if e != nil {
		return out, e
	}
	paths, db, history, log, e := dependencies(p, cfg)
	if e != nil {
		return out, e
	}
	if len(paths) > MaxFiles {
		return out, failure("limit_exceeded")
	}
	out.Manifest = Manifest{Kind: "frp-server-context", Version: 1, ConfigFile: p.ConfigFile, CWD: p.WorkingDir, PolicyDigest: hash(policyBytes(p)), DatabaseFile: db, HistoryPath: history, HistoryEnabled: history != "", LogPath: log, TemplateRequirements: requirements, Files: []File{}}
	total := 0
	for i, path := range paths {
		if ctx.Err() != nil {
			return out, failure("cancelled")
		}
		m, e := read(path)
		if e != nil {
			return out, e
		}
		total += len(m.data)
		if total > MaxTotalBytes {
			return out, failure("limit_exceeded")
		}
		payload := fmt.Sprintf("context/f%06d", i+1)
		out.payload[path] = m
		out.Manifest.Files = append(out.Manifest.Files, File{path, payload, int64(len(m.data)), hash(m.data), m.mtime})
	}
	raw, _ := json.Marshal(out.Manifest)
	out.Manifest.ContextRevision = hash(raw)
	return out, nil
}
func (s Snapshot) protect(paths []string) error {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if !absolute(path) {
			return failure("input_invalid")
		}
		if reserved(path, s.Manifest.DatabaseFile) || path == s.Manifest.DatabaseFile || path == s.Manifest.LogPath || s.Manifest.HistoryEnabled && within(path, s.Manifest.HistoryPath) {
			return failure("input_conflict")
		}
		for _, f := range s.Manifest.Files {
			if path == f.Path {
				return failure("input_conflict")
			}
		}
	}
	return nil
}
func (s Snapshot) Summary() Summary {
	data, _ := json.Marshal(s.Manifest)
	n := int64(0)
	for _, f := range s.Manifest.Files {
		n += f.Size
	}
	return Summary{"ok", 1, hash(data), s.Manifest.ContextRevision, len(s.Manifest.Files), n}
}
func WriteSnapshot(path string, s Snapshot) (Summary, error) {
	empty := Summary{}
	if !absolute(path) {
		return empty, failure("output_invalid")
	}
	parent, e := openNoFollow(filepath.Dir(path), true)
	if e != nil {
		return empty, e
	}
	defer parent.Close()
	if e = unix.Mkdirat(int(parent.Fd()), filepath.Base(path), 0700); e != nil {
		return empty, failure("output_exists")
	}
	fd, e := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return empty, failure("output_invalid")
	}
	root := os.NewFile(uintptr(fd), path)
	defer root.Close()
	if e = unix.Mkdirat(fd, "context", 0700); e != nil {
		return empty, failure("write_failed")
	}
	cfd, e := unix.Openat(fd, "context", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return empty, failure("write_failed")
	}
	defer unix.Close(cfd)
	write := func(dir int, name string, data []byte) error {
		dest, e := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil {
			return failure("write_failed")
		}
		f := os.NewFile(uintptr(dest), name)
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil || ce != nil {
			return failure("write_failed")
		}
		return nil
	}
	for _, file := range s.Manifest.Files {
		if e = write(cfd, filepath.Base(file.Payload), s.payload[file.Path].data); e != nil {
			return empty, e
		}
	}
	data, _ := json.Marshal(s.Manifest)
	if e = write(fd, "POLICY.json", policyBytes(s.Policy)); e == nil {
		e = write(fd, "CONTEXT.json", data)
	}
	if e != nil {
		return empty, e
	}
	if unix.Fsync(cfd) != nil || root.Sync() != nil || parent.Sync() != nil {
		return empty, failure("write_failed")
	}
	named, e := openNoFollow(path, true)
	if e != nil {
		return empty, failure("source_changed")
	}
	defer named.Close()
	a, _ := root.Stat()
	b, _ := named.Stat()
	if a == nil || b == nil || !os.SameFile(a, b) {
		return empty, failure("source_changed")
	}
	return s.Summary(), nil
}
func ReadSnapshot(ctx context.Context, path, digestValue string, p Policy, env map[string]string) (Snapshot, error) {
	var empty Snapshot
	p, e := normalize(p)
	if e != nil {
		return empty, e
	}
	if !digest(digestValue) {
		return empty, failure("manifest_invalid")
	}
	for _, directory := range []string{path, filepath.Join(path, "context")} {
		f, err := openNoFollow(directory, true)
		if err != nil {
			return empty, failure("payload_invalid")
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return empty, failure("payload_invalid")
		}
		native, ok := info.Sys().(*syscall.Stat_t)
		if !ok || native.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0700 {
			return empty, failure("payload_invalid")
		}
	}
	manifest, e := readPrivate(filepath.Join(path, "CONTEXT.json"), true)
	if e != nil || hash(manifest.data) != digestValue {
		return empty, failure("manifest_invalid")
	}
	var m Manifest
	if canonical(manifest.data, &m) != nil || m.Version != 1 || m.Kind != "frp-server-context" || len(m.Files) > MaxFiles || len(m.Files) < 1 || m.TemplateRequirements == nil || !digest(m.ContextRevision) {
		return empty, failure("manifest_invalid")
	}
	pol, e := readPrivate(filepath.Join(path, "POLICY.json"), true)
	if e != nil || !bytes.Equal(pol.data, policyBytes(p)) {
		return empty, failure("policy_mismatch")
	}
	materialized := map[string]material{}
	expected := map[string]bool{"CONTEXT.json": true, "POLICY.json": true, "context": true}
	total := int64(0)
	for i, f := range m.Files {
		if f.Payload != fmt.Sprintf("context/f%06d", i+1) || !p.authorized(f.Path, false) || materialized[f.Path].data != nil || f.Size < 0 || f.Size > MaxFileBytes || !digest(f.SHA256) {
			return empty, failure("manifest_invalid")
		}
		value, e := readPrivate(filepath.Join(path, f.Payload), true)
		if e != nil || int64(len(value.data)) != f.Size || hash(value.data) != f.SHA256 {
			return empty, failure("payload_invalid")
		}
		total += f.Size
		if total > MaxTotalBytes {
			return empty, failure("limit_exceeded")
		}
		value.mtime = f.MtimeNS
		materialized[f.Path] = value
		expected[f.Payload] = true
	}
	// A sealed directory contains exactly the declared ordinary context, never
	// database snapshots or arbitrary archive-provided paths.
	for _, directory := range []string{"", "context"} {
		d, e := openNoFollow(filepath.Join(path, directory), true)
		if e != nil {
			return empty, e
		}
		names, e := d.Readdirnames(MaxFiles + 4)
		d.Close()
		if e != nil && e != io.EOF {
			return empty, failure("payload_invalid")
		}
		for _, name := range names {
			key := filepath.Join(directory, name)
			if !expected[key] {
				return empty, failure("payload_invalid")
			}
			delete(expected, key)
		}
	}
	if len(expected) != 0 {
		return empty, failure("payload_invalid")
	}
	rebuilt, e := build(ctx, p, env, func(path string) (material, error) {
		v, ok := materialized[path]
		if !ok {
			return material{}, failure("dependency_missing")
		}
		return v, nil
	})
	if e != nil {
		return empty, e
	}
	encoded, _ := json.Marshal(rebuilt.Manifest)
	if !bytes.Equal(encoded, manifest.data) || len(rebuilt.payload) != len(materialized) {
		return empty, failure("manifest_mismatch")
	}
	return rebuilt, nil
}
