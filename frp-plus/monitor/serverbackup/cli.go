package serverbackup

import (
	"context"
	"encoding/json"
	"path/filepath"
)

type CLIRequest struct {
	Action, ConfigFile, PolicyFile, EnvFile, SourcePolicyFile, SourceEnvFile, Checkpoint, ManifestDigest, Output string
	Offline                                                                                                      bool
}

func loadInputs(policy, envPath string) (Policy, map[string]string, error) {
	data, e := readPrivate(policy, true)
	if e != nil {
		return Policy{}, nil, failure("policy_invalid")
	}
	p, e := DecodePolicy(data.data)
	if e != nil {
		return p, nil, e
	}
	env := map[string]string{}
	if envPath != "" {
		raw, e := readPrivate(envPath, true)
		if e != nil || canonical(raw.data, &env) != nil || len(env) > 128 {
			return p, nil, failure("environment_invalid")
		}
		for k, v := range env {
			if len(k) < 1 || len(k) > 128 || len(v) > 65536 {
				return p, nil, failure("environment_invalid")
			}
		}
	}
	return p, env, nil
}
func RunCLI(ctx context.Context, r CLIRequest) (Summary, error) {
	if !r.Offline || !absolute(r.Output) || !absolute(r.PolicyFile) {
		return Summary{}, failure("arguments_invalid")
	}
	p, env, e := loadInputs(r.PolicyFile, r.EnvFile)
	if e != nil {
		return Summary{}, e
	}
	forbidden := []string{r.PolicyFile, r.EnvFile, r.SourcePolicyFile, r.SourceEnvFile}
	// Protect all explicit inputs from accidental ordinary dependency capture or
	// output replacement. Environment maps are never archive material.
	for _, v := range forbidden {
		if v != "" && (!absolute(v) || within(v, r.Output)) {
			return Summary{}, failure("input_conflict")
		}
	}
	var s Snapshot
	switch r.Action {
	case "capture":
		if r.ConfigFile != p.ConfigFile || r.Checkpoint != "" || r.SourcePolicyFile != "" || r.SourceEnvFile != "" || r.ManifestDigest != "" {
			return Summary{}, failure("arguments_invalid")
		}
		s, e = Capture(ctx, p, env, forbidden)
	case "transform":

		if within(r.Output, r.Checkpoint) || within(r.Checkpoint, r.Output) {
			return Summary{}, failure("output_conflict")
		}
		for _, input := range forbidden {
			if input == "" {
				continue
			}
			for _, root := range p.Roots {
				if within(input, root.Path) {
					return Summary{}, failure("input_conflict")
				}
			}
			for _, file := range p.Files {
				if input == file.Path {
					return Summary{}, failure("input_conflict")
				}
			}
		}
		if r.ConfigFile != "" || !absolute(r.Checkpoint) || !absolute(r.SourcePolicyFile) || !digest(r.ManifestDigest) {
			return Summary{}, failure("arguments_invalid")
		}
		sourceEnvPath := r.SourceEnvFile
		if sourceEnvPath == "" {
			sourceEnvPath = r.EnvFile
		}
		sp, se, loadErr := loadInputs(r.SourcePolicyFile, sourceEnvPath)
		if loadErr != nil {
			return Summary{}, loadErr
		}
		source, readErr := ReadSnapshot(ctx, r.Checkpoint, r.ManifestDigest, sp, se)
		if readErr != nil {
			return Summary{}, readErr
		}
		if readErr = source.protect(forbidden); readErr != nil {
			return Summary{}, readErr
		}
		s, e = Transform(ctx, source, p, se, env, forbidden)
	default:
		return Summary{}, failure("arguments_invalid")
	}
	if e != nil {
		return Summary{}, e
	}
	if s.Manifest.HistoryEnabled && within(r.Output, s.Manifest.HistoryPath) || within(s.Manifest.DatabaseFile, r.Output) || within(s.Manifest.LogPath, r.Output) {
		return Summary{}, failure("output_conflict")
	}
	for _, f := range s.Manifest.Files {
		if within(f.Path, r.Output) || f.Path == r.Output {
			return Summary{}, failure("output_conflict")
		}
	}
	// A generated temporary context is independent of the installation. The
	// archive installer chooses when to publish it under its stopped-process gate.
	return WriteSnapshot(filepath.Clean(r.Output), s)
}
func MarshalSummary(s Summary) []byte { data, _ := json.Marshal(s); return append(data, '\n') }
