package managed

import (
	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	"path/filepath"
	"strings"
)

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func canonicalAbsolute(path string) bool {
	return len(path) > 0 && len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}
func (p restoreInstallPlan) validTarget(path string) bool {
	if p.Version == 1 {
		return validPlanTarget(path)
	}
	if strings.HasPrefix(path, "managed/") {
		return validPlanTarget(path)
	}
	if !canonicalAbsolute(path) {
		return false
	}
	for _, root := range p.TargetRoots {
		if canonicalAbsolute(root) && filepath.Dir(root) != root && path != root && pathWithin(path, root) {
			return true
		}
	}
	for _, file := range p.TargetFiles {
		if canonicalAbsolute(file) && file == path {
			return true
		}
	}
	return false
}
func planWalk(installation *privateDir, plan restoreInstallPlan, path string, create bool) (*privateDir, string, func(), error) {
	if !plan.validTarget(path) {
		return nil, "", func() {}, ErrUnsafePath
	}
	if plan.Version == 1 || strings.HasPrefix(path, "managed/") {
		return v2Walk(installation, path, create)
	}
	rootPath := ""
	rel := ""
	for _, root := range plan.TargetRoots {
		if path != root && pathWithin(path, root) {
			rootPath = root
			rel, _ = filepath.Rel(root, path)
			break
		}
	}
	if rootPath == "" {
		for _, file := range plan.TargetFiles {
			if file == path {
				rootPath = filepath.Dir(file)
				rel = filepath.Base(file)
				break
			}
		}
	}
	if rootPath == "" {
		return nil, "", func() {}, ErrUnsafePath
	}
	root, err := openExistingPrivateDir(rootPath)
	if err != nil {
		return nil, "", func() {}, err
	}
	dir, name, done, err := v2Walk(root, filepath.ToSlash(rel), create)
	return dir, name, func() { done(); root.close() }, err
}
func planReadTarget(installation *privateDir, plan restoreInstallPlan, path string, limit int64) (StoreSnapshot, int64, error) {
	dir, name, done, err := planWalk(installation, plan, path, false)
	defer done()
	if err != nil {
		return StoreSnapshot{}, 0, err
	}
	if dir == nil {
		return StoreSnapshot{}, 0, nil
	}
	return dir.readMetadata(name, limit)
}

func checkMappedIncludeTargets(plan restoreInstallPlan, installation *privateDir, graphBytes []byte) error {
	graph, err := backupmanifest.Decode(graphBytes)
	if err != nil {
		return ErrRecovery
	}
	for _, include := range graph.Includes {
		dir, _, done, err := planWalk(installation, plan, filepath.Join(filepath.Dir(include.Pattern), ".include-placeholder"), false)
		if err != nil {
			done()
			return err
		}
		if dir == nil {
			done()
			continue
		}
		names, err := v2MatchingFiles(dir, filepath.Base(include.Pattern))
		done()
		if err != nil {
			return err
		}
		expected := map[string]bool{}
		for _, path := range include.Files {
			expected[filepath.Base(path)] = true
		}
		for _, name := range names {
			if !expected[name] {
				return ErrConflict
			}
		}
	}
	return nil
}

// Roots authorize descendants; exact files never grant access to their parent.
func (p restoreInstallPlan) validAuthorization() bool {
	if len(p.TargetRoots) == 0 || len(p.TargetRoots)+len(p.TargetFiles) > 128 {
		return false
	}
	for i, root := range p.TargetRoots {
		if !canonicalAbsolute(root) || filepath.Dir(root) == root {
			return false
		}
		for _, earlier := range p.TargetRoots[:i] {
			if pathWithin(root, earlier) || pathWithin(earlier, root) {
				return false
			}
		}
	}
	for i, file := range p.TargetFiles {
		if !canonicalAbsolute(file) || filepath.Dir(file) == file {
			return false
		}
		for _, root := range p.TargetRoots {
			if pathWithin(file, root) || pathWithin(root, file) {
				return false
			}
		}
		for _, earlier := range p.TargetFiles[:i] {
			if pathWithin(file, earlier) || pathWithin(earlier, file) {
				return false
			}
		}
	}
	return true
}
func planAuthorizationMatches(p restoreInstallPlan, target RelocationContextV3) bool {
	if p.Version != 2 || len(p.TargetRoots) != len(target.TargetRoots) || len(p.TargetFiles) != len(target.TargetFiles) {
		return false
	}
	for i, v := range p.TargetRoots {
		if v != target.TargetRoots[i] {
			return false
		}
	}
	for i, v := range p.TargetFiles {
		if v != target.TargetFiles[i] {
			return false
		}
	}
	return p.validAuthorization()
}
func v2TargetLimit(entry restorePlanEntry) int64 {
	if entry.Target == "managed/"+contextHistoryName {
		return contextHistoryLimit
	}
	return v2EntryLimit(entry.Payload)
}

func checkRelocationTargetHistory(root *privateDir) error {
	operations, err := root.optionalSubdir("operations")
	if err != nil || operations == nil {
		return err
	}
	defer operations.close()
	names, err := operations.names()
	if err != nil {
		return err
	}
	for _, name := range names {
		if !validCheckpointPath("managed/operations/" + name) {
			return ErrRecovery
		}
		if filepath.Ext(name) != ".json" {
			continue
		}
		data, err := operations.read(name, MaxCheckpointFileBytes)
		if err != nil {
			return err
		}
		record, _, err := decodeManagedRecord(data.Bytes, name)
		if err != nil {
			return err
		}
		if !terminal(record.State) || record.NeedsRuntime {
			return ErrBusy
		}
	}
	return nil
}
