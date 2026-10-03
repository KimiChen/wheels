package backupmanifest

import (
	"bytes"
	"testing"
)

func TestManifestStrictEnvelope(t *testing.T) {
	valid := []byte(`{"version":2,"kind":"frp-agent-dependency-graph","policy_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config_file":"/config","working_dir":"/cwd","store_file":"/store","files":[{}],"directories":[],"includes":[],"references":[]}`)
	if _, err := Decode(valid); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"duplicate":          bytes.Replace(valid, []byte(`"version":2`), []byte(`"version":2,"version":2`), 1),
		"case-alias-version": bytes.Replace(valid, []byte(`"version":2`), []byte(`"Version":2,"version":2`), 1),
		"case-alias-path":    bytes.Replace(valid, []byte(`"config_file":"/config"`), []byte(`"CONFIG_FILE":"/other","config_file":"/config"`), 1),
		"case-alias-nested":  bytes.Replace(valid, []byte(`"files":[{}]`), []byte(`"files":[{"Size":0,"size":0}]`), 1),
		"unicode-fold-alias": bytes.Replace(valid, []byte(`"version":2`), []byte(`"verſion":2`), 1),
		"unknown":            bytes.Replace(valid, []byte(`"version":2`), []byte(`"version":2,"other":true`), 1),
		"version":            bytes.Replace(valid, []byte(`"version":2`), []byte(`"version":3`), 1),
		"trailing":           append(append([]byte(nil), valid...), []byte(` {}`)...),
		"null-arrays":        bytes.Replace(valid, []byte(`"includes":[]`), []byte(`"includes":null`), 1),
		"oversize":           bytes.Repeat([]byte(" "), MaxManifestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(data); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
}

func TestManifestTemplateRequirements(t *testing.T) {
	value := Manifest{Version: TemplateVersion, Kind: Kind, PolicyDigest: Digest([]byte("policy")), ConfigFile: "/config", WorkingDir: "/cwd", StoreFile: "/store", Files: []File{{Path: "/config", Kind: "main", Exists: true}}, Directories: []Directory{}, Includes: []Include{}, References: []Reference{}, TemplateRequirements: []TemplateRequirement{{From: "/config", Names: []string{"HOST", "TOKEN"}, RenderedSHA256: Digest([]byte("rendered"))}}}
	valid, err := Encode(value)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"old-version":        func(v *Manifest) { v.Version = Version },
		"unknown-version":    func(v *Manifest) { v.Version = TemplateVersion + 1 },
		"requirements-empty": func(v *Manifest) { v.TemplateRequirements = nil },
		"duplicate-source":   func(v *Manifest) { v.TemplateRequirements = append(v.TemplateRequirements, v.TemplateRequirements[0]) },
		"foreign-source":     func(v *Manifest) { v.TemplateRequirements[0].From = "/outside" },
		"store-template":     func(v *Manifest) { v.Files[0].Kind = "store" },
		"missing-source":     func(v *Manifest) { v.Files[0].Exists = false },
		"names-null":         func(v *Manifest) { v.TemplateRequirements[0].Names = nil },
		"names-duplicate":    func(v *Manifest) { v.TemplateRequirements[0].Names = []string{"HOST", "HOST"} },
		"names-unsorted":     func(v *Manifest) { v.TemplateRequirements[0].Names = []string{"TOKEN", "HOST"} },
		"names-secret-text":  func(v *Manifest) { v.TemplateRequirements[0].Names = []string{"secret\nvalue"} },
		"names-excessive":    func(v *Manifest) { v.TemplateRequirements[0].Names = make([]string, MaxTemplateNames+1) },
		"digest-invalid":     func(v *Manifest) { v.TemplateRequirements[0].RenderedSHA256 = "plain-secret" },
	} {
		t.Run(name, func(t *testing.T) {
			v, err := Decode(valid)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&v)
			if _, err = Encode(v); err == nil {
				t.Fatal("invalid requirements accepted")
			}
		})
	}
	for name, field := range map[string]string{
		"case-alias":         `"Names":["TOKEN"],"names"`,
		"duplicate":          `"names":["TOKEN"],"names"`,
		"environment-values": `"values":{"TOKEN":"secret"},"names"`,
		"rendered-config":    `"rendered":"secret","names"`,
	} {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Replace(valid, []byte(`"names"`), []byte(field), 1)
			if _, err := Decode(bad); err == nil {
				t.Fatal("unknown or duplicate template data accepted")
			}
		})
	}
}
