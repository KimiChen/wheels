package configuration

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	nativeconfig "github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

type dependency struct {
	Path, Kind, State string
	Data              []byte
	Mode              uint32
	Modified          int64
}
type includeMatch struct {
	Pattern string
	Files   []string
}
type snapshot struct {
	in                      Input
	files, store            []object
	fileMemory, storeMemory []object
	deps                    []dependency
	patterns                []includeMatch
	issues                  []Issue
	conflicts               map[string]bool
	bytes                   int
	revision                string
	contextRevision         string
	referencedEnv           map[string]string
	originalStore           []byte
	originalStoreExists     bool
	// Staged private materials are used only by offline restore closure validation.
	material map[string]dependency
}

func (s *snapshot) issue(code string) {
	for _, item := range s.issues {
		if item.Code == code {
			return
		}
	}
	s.issues = append(s.issues, Issue{Code: code})
}
func code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return "invalid_source"
}

func (s *snapshot) resolve(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(s.in.WorkingDir, path)
}

func regularNoLinks(path string) ([]byte, os.FileInfo, error) {
	for cursor := path; ; cursor = filepath.Dir(cursor) {
		info, err := os.Lstat(cursor)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, failure("unsupported_dependency")
		}
		if filepath.Dir(cursor) == cursor {
			break
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, info, failure("unsupported_dependency")
	}
	if info.Size() > MaxInputBytes {
		return nil, info, failure("limit_exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxInputBytes+1))
	if err != nil {
		return nil, info, err
	}
	if len(data) > MaxInputBytes {
		return nil, info, failure("limit_exceeded")
	}
	after, err := f.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, info, failure("source_changed")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		return nil, info, failure("source_changed")
	}
	return data, info, nil
}

func (s *snapshot) read(path, kind string, missingOK bool) ([]byte, bool) {
	path = s.resolve(path)
	for _, dep := range s.deps {
		if dep.Path == path && (dep.Kind == kind || dep.State == "ready") {
			return dep.Data, dep.State == "ready"
		}
	}
	d := dependency{Path: path, Kind: kind, State: "ready"}
	if len(s.deps) >= MaxDependencies {
		s.issue("limit_exceeded")
		return nil, false
	}
	if kind != "store" && s.in.StoreFile != "" && path == s.resolve(s.in.StoreFile) {
		// A main/include/credential dependency on the Store is ambiguous. Keep
		// its fixed path in context without reading Store during recovery.
		d.State = "unavailable"
		s.deps = append(s.deps, d)
		s.issue("unsupported_dependency")
		return nil, false
	}
	var data []byte
	var info os.FileInfo
	var err error
	if s.material != nil {
		staged, ok := s.material[path]
		if !ok {
			err = os.ErrNotExist
		} else {
			data = append([]byte(nil), staged.Data...)
			d.Mode, d.Modified = staged.Mode, staged.Modified
		}
	} else {
		data, info, err = regularNoLinks(path)
	}
	if info != nil {
		d.Mode = uint32(info.Mode())
		d.Modified = info.ModTime().UnixNano()
	}
	if err != nil {
		d.State = "unavailable"
		if os.IsNotExist(err) {
			d.State = "missing"
			if !missingOK {
				s.issue("unsupported_dependency")
			}
		} else {
			s.issue("unsupported_dependency")
		}
	} else {
		d.Data = data
		s.bytes += len(data)
		if s.bytes > MaxInputBytes {
			s.issue("limit_exceeded")
		}
	}
	s.deps = append(s.deps, d)
	return data, err == nil
}

func (s *snapshot) parseFile(path, origin string) (*v1.ClientCommonConfig, []object) {
	data, ok := s.read(path, origin, false)
	if !ok {
		return nil, nil
	}
	if nativeconfig.DetectLegacyINIFormat(data) {
		s.issue("legacy_read_only")
		return nil, nil
	}
	if err := s.templateDependencies(data); err != nil {
		s.issue(code(err))
		return nil, nil
	}
	rendered, err := nativeconfig.RenderWithTemplate(data, &nativeconfig.Values{Envs: s.in.TemplateEnv})
	if err != nil {
		s.issue("invalid_template")
		return nil, nil
	}
	if len(rendered) > MaxInputBytes {
		s.issue("limit_exceeded")
		return nil, nil
	}
	if strings.HasPrefix(strings.TrimSpace(string(rendered)), "{") {
		if err := strictJSON(rendered); err != nil {
			s.issue(code(err))
			return nil, nil
		}
	}
	cfg := v1.ClientConfig{}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if format == "yml" {
		format = "yaml"
	}
	if err := nativeconfig.LoadConfigure(rendered, &cfg, true, format); err != nil {
		s.issue("unsupported_source")
		return nil, nil
	}
	values := Objects{Proxies: []v1.ProxyConfigurer{}, Visitors: []v1.VisitorConfigurer{}}
	for _, p := range cfg.Proxies {
		values.Proxies = append(values.Proxies, p.ProxyConfigurer)
	}
	for _, v := range cfg.Visitors {
		values.Visitors = append(values.Visitors, v.VisitorConfigurer)
	}
	objects, err := fromNative(values, origin)
	if err != nil {
		s.issue(code(err))
		return nil, nil
	}
	return &cfg.ClientCommonConfig, objects
}

func (s *snapshot) includes(patterns []string, parse bool) []object {
	out := []object{}
	for _, pattern := range patterns {
		absolute := s.resolve(pattern)
		dir := filepath.Dir(absolute)
		match := includeMatch{Pattern: absolute, Files: []string{}}
		// Native includes match only the basename inside a literal directory.
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil || resolved != dir {
			s.issue("unsupported_dependency")
			s.patterns = append(s.patterns, match)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			s.issue("unsupported_dependency")
			s.patterns = append(s.patterns, match)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ok, err := filepath.Match(filepath.Base(absolute), entry.Name())
			if err != nil {
				s.issue("unsupported_dependency")
				break
			}
			if !ok {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			match.Files = append(match.Files, path)
			if parse {
				_, objects := s.parseFile(path, "include")
				out = append(out, objects...)
			} else {
				s.read(path, "include", false)
			}
		}
		s.patterns = append(s.patterns, match)
	}
	return out
}

func commonCopy(value *v1.ClientCommonConfig) (*v1.ClientCommonConfig, error) {
	if value == nil {
		return nil, failure("invalid_memory")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, failure("invalid_memory")
	}
	out := &v1.ClientCommonConfig{}
	if json.Unmarshal(data, out) != nil {
		return nil, failure("invalid_memory")
	}
	return out, nil
}
func sameJSON(a, b any) bool {
	left, e1 := json.Marshal(a)
	right, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(left) == string(right)
}

func (s *snapshot) dependencies(raw map[string]json.RawMessage, common bool) {
	paths := []string{"plugin.crtPath", "plugin.keyPath"}
	if common {
		paths = []string{"transport.tls.certFile", "transport.tls.keyFile", "transport.tls.trustedCaFile", "webServer.tls.certFile", "webServer.tls.keyFile", "webServer.tls.trustedCaFile", "auth.oidc.trustedCaFile", "auth.tokenSource.file.path", "auth.oidc.tokenSource.file.path", "telemetry.tokenFile", "telemetry.caFile"}
	}
	for _, path := range paths {
		if raw, ok := getPath(raw, path); ok {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				s.issue("unsupported_dependency")
			} else if value != "" {
				s.read(value, "local_file", false)
			}
		}
	}
	dynamic := []string{"plugin.localPath", "plugin.unixPath"}
	if common {
		dynamic = []string{"webServer.assetsDir"}
	}
	for _, path := range dynamic {
		if raw, ok := getPath(raw, path); ok {
			var value string
			_ = json.Unmarshal(raw, &value)
			if value != "" {
				s.issue("unsupported_dependency")
			}
		}
	}
	if common {
		for _, path := range []string{"auth.tokenSource.type", "auth.oidc.tokenSource.type"} {
			if raw, ok := getPath(raw, path); ok {
				var value string
				_ = json.Unmarshal(raw, &value)
				if value == "exec" {
					s.issue("unsupported_dependency")
				}
			}
		}
	}
}

func takeSnapshot(input Input, contextOnly ...bool) (*snapshot, error) {
	return takeSnapshotMaterials(input, nil, contextOnly...)
}

func takeSnapshotMaterials(input Input, material map[string]dependency, contextOnly ...bool) (*snapshot, error) {
	s := &snapshot{material: material, in: input, deps: []dependency{}, patterns: []includeMatch{}, issues: []Issue{}, conflicts: map[string]bool{}, referencedEnv: map[string]string{}}
	var err error
	if input.WorkingDir == "" {
		s.in.WorkingDir, err = os.Getwd()
		if err != nil {
			return nil, failure("invalid_memory")
		}
	} else {
		s.in.WorkingDir, err = filepath.Abs(input.WorkingDir)
		if err != nil {
			return nil, failure("invalid_memory")
		}
	}
	s.in.StartupCommon, err = commonCopy(input.StartupCommon)
	if err != nil {
		return nil, err
	}
	s.in.ReloadCommon, err = commonCopy(input.ReloadCommon)
	if err != nil {
		return nil, err
	}
	envs := input.TemplateEnv
	if envs == nil {
		envs = nativeconfig.GetValues().Envs
	}
	s.in.TemplateEnv = map[string]string{}
	for key, value := range envs {
		s.in.TemplateEnv[key] = value
	}
	s.in.UnsafeFeatures = append([]string(nil), input.UnsafeFeatures...)
	sort.Strings(s.in.UnsafeFeatures)
	s.fileMemory, err = fromNative(input.FileMemory, "file")
	if err != nil {
		return nil, err
	}
	var diskCommon *v1.ClientCommonConfig
	if input.ConfigFile != "" {
		diskCommon, s.files = s.parseFile(input.ConfigFile, "file")
		if diskCommon != nil {
			s.files = append(s.files, s.includes(diskCommon.IncludeConfigFiles, true)...)
			// Complete reads http_proxy implicitly. Restore the explicitly captured
			// default afterwards; no process environment is changed by this package.
			proxy := diskCommon.Transport.ProxyURL
			if proxy == "" {
				proxy = input.HTTPProxy
			}
			if diskCommon.Complete() != nil {
				s.issue("invalid_source")
			}
			diskCommon.Transport.ProxyURL = proxy
			if !sameJSON(diskCommon, s.in.ReloadCommon) {
				s.issue("source_drift")
			}
		}
	} else if len(s.fileMemory) > 0 {
		s.issue("unsupported_source")
	}
	// Previously loaded include patterns are also part of CAS if disk drifted.
	if diskCommon == nil || !sameJSON(diskCommon.IncludeConfigFiles, s.in.ReloadCommon.IncludeConfigFiles) {
		s.includes(s.in.ReloadCommon.IncludeConfigFiles, false)
	}
	a, _ := fingerprint(s.files)
	b, _ := fingerprint(s.fileMemory)
	if a != b {
		s.issue("source_drift")
	}
	seen := map[string]bool{}
	for _, o := range s.files {
		if seen[o.key()] {
			s.issue("duplicate_name")
		}
		seen[o.key()] = true
	}
	if len(s.files) > MaxObjects {
		s.issue("limit_exceeded")
	}
	for _, c := range []*v1.ClientCommonConfig{s.in.StartupCommon, s.in.ReloadCommon, diskCommon} {
		if c != nil {
			data, _ := json.Marshal(c)
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(data, &raw)
			s.dependencies(raw, true)
		}
	}
	for _, objects := range [][]object{s.files, s.fileMemory} {
		for _, o := range objects {
			s.dependencies(o.raw, false)
		}
	}
	memFile, _ := fingerprint(s.fileMemory)
	// Secret material exists only in this private hash input; no dependency
	// paths, contents, individual secret hashes or environment values are sent.
	payload := struct {
		ConfigFile, StoreFile, CWD, FileMemory, HTTPProxy string
		Startup, Reload                                   *v1.ClientCommonConfig
		Env                                               map[string]string
		Unsafe                                            []string
		Dependencies                                      []dependency
		Includes                                          []includeMatch
	}{
		s.resolve(input.ConfigFile), s.resolve(input.StoreFile), s.in.WorkingDir, memFile, input.HTTPProxy, s.in.StartupCommon, s.in.ReloadCommon, s.referencedEnv, s.in.UnsafeFeatures, s.deps, s.patterns}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, failure("invalid_memory")
	}
	s.contextRevision = hash(data)
	if len(contextOnly) > 0 && contextOnly[0] {
		return s, nil
	}
	s.storeMemory, err = fromNative(input.StoreMemory, "store")
	if err != nil {
		return nil, err
	}
	if input.StoreFile == "" {
		s.issue("store_unavailable")
	} else {
		data, exists := s.read(input.StoreFile, "store", true)
		s.originalStore = append([]byte(nil), data...)
		s.originalStoreExists = exists
		if exists {
			s.store, err = parseStore(data)
			if err != nil {
				s.issue(code(err))
			}
		} else {
			s.store = []object{}
		}
	}
	a, _ = fingerprint(s.store)
	b, _ = fingerprint(s.storeMemory)
	if a != b {
		s.issue("source_drift")
	}
	for _, o := range s.store {
		if seen[o.key()] {
			s.conflicts[o.key()] = true
			s.issue("ownership_conflict")
		}
	}
	if len(s.files)+len(s.store) > MaxObjects {
		s.issue("limit_exceeded")
	}
	for _, objects := range [][]object{s.store, s.storeMemory} {
		for _, o := range objects {
			for _, path := range []string{"plugin.crtPath", "plugin.keyPath", "plugin.localPath", "plugin.unixPath"} {
				if raw, ok := getPath(o.raw, path); ok {
					var value string
					_ = json.Unmarshal(raw, &value)
					if value != "" {
						s.issue("unsupported_dependency")
					}
				}
			}
			s.dependencies(o.raw, false)
		}
	}
	memStore, _ := fingerprint(s.storeMemory)
	full := struct {
		Context, StoreMemory string
		VirtualNetReady      bool
		Dependencies         []dependency
	}{s.contextRevision, memStore, s.in.VirtualNetReady, s.deps}
	data, err = json.Marshal(full)
	if err != nil {
		return nil, failure("invalid_memory")
	}
	s.revision = hash(data)
	return s, nil
}

func (s *snapshot) inspection() *Inspection {
	out := &Inspection{Revision: s.revision, ContextRevision: s.contextRevision, State: "ready", Objects: []ObjectView{}, Dependencies: []DependencyView{}, Issues: append([]Issue{}, s.issues...)}
	if len(s.issues) > 0 {
		out.State = "read_only"
	}
	for _, dep := range s.deps {
		out.Dependencies = append(out.Dependencies, DependencyView{Kind: dep.Kind, State: dep.State})
	}
	objects := append(append([]object{}, s.files...), s.store...)
	sort.SliceStable(objects, func(i, j int) bool { return objects[i].key() < objects[j].key() })
	for _, o := range objects {
		view := viewObject(o, s.in.ReloadCommon.Start, s.conflicts[o.key()])
		if out.State != "ready" {
			view.Writable = false
		}
		out.Objects = append(out.Objects, view)
	}
	return out
}

// Inspect returns a read-only inventory on unsupported/ambiguous source input.
// Even that result has a revision over every input that could be observed.
func Inspect(input Input) (*Inspection, error) {
	s, err := takeSnapshot(input)
	if err != nil {
		return nil, err
	}
	return s.inspection(), nil
}

// InspectContext is the startup recovery guard. It never reads or parses the
// Store or its memory, and fails closed when external sources are incomplete.
func InspectContext(input Input) (string, error) {
	s, err := takeSnapshot(input, true)
	if err != nil {
		return "", err
	}
	if len(s.issues) > 0 {
		return "", failure(s.issues[0].Code)
	}
	return s.contextRevision, nil
}
