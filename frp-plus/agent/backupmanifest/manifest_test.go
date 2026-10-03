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
