package configuration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode/utf8"

	"github.com/fatedier/frp/extension/frpmonitor/agent/backupmanifest"
	nativeconfig "github.com/fatedier/frp/pkg/config"
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

const backupTemplateSteps = 16384

// RenderBackupTemplate renders only statically named environment references.
// It never reads os.Environ and never returns native diagnostics (which may
// contain secrets). The returned rendered bytes are ephemeral private input
// for decoding, never checkpoint payload. Callers archive the original bytes.
func RenderBackupTemplate(ctx context.Context, data []byte, env map[string]string) ([]byte, []string, error) {
	if len(data) > backupmanifest.MaxFileBytes || !utf8.Valid(data) {
		return nil, nil, backupErr("limit_exceeded")
	}
	names, values := []string{}, map[string]string{}
	var total int
	err := walkTemplateDependencies(data, func(key string) error {
		if !backupmanifest.ValidTemplateName(key) {
			return failure("unsupported_dependency")
		}
		if _, exists := values[key]; exists {
			return nil
		}
		value, exists := env[key]
		if !exists {
			return failure("unsupported_dependency")
		}
		if len(values) >= backupmanifest.MaxTemplateNames || len(value) > 65536 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return failure("limit_exceeded")
		}
		total += len(value)
		if total > backupmanifest.MaxFileBytes {
			return failure("limit_exceeded")
		}
		names, values[key] = append(names, key), value
		return nil
	})
	if err != nil {
		return nil, nil, backupErr(code(err))
	}
	sort.Strings(names)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	functions := template.FuncMap{
		"parseNumberRange":     backupNumberRange,
		"parseNumberRangePair": backupNumberRangePair,
		"printf":               backupTemplatePrintf,
		"print":                func(args ...any) (string, error) { return backupTemplatePrint(false, args...) },
		"println":              func(args ...any) (string, error) { return backupTemplatePrint(true, args...) },
		"html":                 func(args ...any) (string, error) { return backupTemplateEscape(template.HTMLEscapeString, args...) },
		"js":                   func(args ...any) (string, error) { return backupTemplateEscape(template.JSEscapeString, args...) },
		"urlquery": func(args ...any) (string, error) {
			return backupTemplateEscape(func(s string) string { return template.URLQueryEscaper(s) }, args...)
		},
	}
	t, err := template.New("frp").Funcs(functions).Parse(string(data))
	if err != nil {
		return nil, nil, backupErr("invalid_template")
	}
	steps := 0
	// Insert an execution budget into every list, including empty range bodies
	// and named templates. A bounded writer alone cannot stop empty recursion
	// or nested ranges that perform no output.
	t.Funcs(template.FuncMap{"__backupStep": func() (string, error) {
		steps++
		if ctx.Err() != nil || steps > backupTemplateSteps {
			return "", backupErr("limit_exceeded")
		}
		return "", nil
	}})
	probe, _ := template.New("budget").Funcs(template.FuncMap{"__backupStep": func() string { return "" }}).Parse("{{__backupStep}}")
	for _, defined := range t.Templates() {
		if defined.Tree != nil {
			backupInstrumentTemplate(defined.Tree.Root, probe.Tree.Root.Nodes[0])
		}
	}
	out := &backupTemplateBuffer{ctx: ctx}
	if err = t.Execute(out, &nativeconfig.Values{Envs: values}); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if steps > backupTemplateSteps || out.exceeded {
			return nil, nil, backupErr("limit_exceeded")
		}
		return nil, nil, backupErr("invalid_template")
	}
	return out.Bytes(), names, nil
}

func backupInstrumentTemplate(list *parse.ListNode, probe parse.Node) {
	if list == nil {
		return
	}
	nodes := make([]parse.Node, 0, 1+len(list.Nodes)*2)
	nodes = append(nodes, probe.Copy())
	for _, node := range list.Nodes {
		nodes = append(nodes, node, probe.Copy())
		switch n := node.(type) {
		case *parse.IfNode:
			backupInstrumentTemplate(n.List, probe)
			backupInstrumentTemplate(n.ElseList, probe)
		case *parse.RangeNode:
			backupInstrumentTemplate(n.List, probe)
			backupInstrumentTemplate(n.ElseList, probe)
		case *parse.WithNode:
			backupInstrumentTemplate(n.List, probe)
			backupInstrumentTemplate(n.ElseList, probe)
		}
	}
	list.Nodes = nodes
}

type backupTemplateBuffer struct {
	bytes.Buffer
	ctx      context.Context
	exceeded bool
}

func (b *backupTemplateBuffer) Write(p []byte) (int, error) {
	if b.ctx.Err() != nil {
		return 0, b.ctx.Err()
	}
	if len(p) > backupmanifest.MaxFileBytes-b.Len() {
		b.exceeded = true
		return 0, io.ErrShortWrite
	}
	return b.Buffer.Write(p)
}

// The native helpers allocate their entire range before rendering. Preserve
// their syntax and order but bound allocation before appending any range.
func backupNumberRange(value string) ([]int64, error) {
	out := []int64{}
	for part := range strings.SplitSeq(strings.TrimSpace(value), ",") {
		pieces := strings.Split(part, "-")
		if len(pieces) < 1 || len(pieces) > 2 {
			return nil, backupErr("invalid_template")
		}
		first, err := strconv.ParseInt(strings.TrimSpace(pieces[0]), 10, 64)
		if err != nil {
			return nil, backupErr("invalid_template")
		}
		last := first
		if len(pieces) == 2 {
			last, err = strconv.ParseInt(strings.TrimSpace(pieces[1]), 10, 64)
			if err != nil || last < first {
				return nil, backupErr("invalid_template")
			}
		}
		// Both bounds are nonnegative in native range syntax. Subtraction can
		// therefore not wrap; +1 is intentionally avoided near MaxInt64.
		if first < 0 || last-first >= int64(MaxObjects-len(out)) {
			return nil, backupErr("limit_exceeded")
		}
		for current := first; ; current++ {
			out = append(out, current)
			if current == last {
				break
			}
		}
	}
	return out, nil
}
func backupNumberRangePair(first, second string) ([]nativeconfig.NumberPair, error) {
	a, err := backupNumberRange(first)
	if err != nil {
		return nil, err
	}
	b, err := backupNumberRange(second)
	if err != nil {
		return nil, err
	}
	if len(a) != len(b) {
		return nil, backupErr("invalid_template")
	}
	out := make([]nativeconfig.NumberPair, len(a))
	for i := range a {
		out[i] = nativeconfig.NumberPair{First: a[i], Second: b[i]}
	}
	return out, nil
}

func backupTemplateArgs(args []any) bool {
	total := 0
	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			total += len(v)
		case []int64:
			total += len(v) * 24
		case []nativeconfig.NumberPair:
			total += len(v) * 50
		default:
			total += 64
		}
		if total > backupmanifest.MaxFileBytes {
			return false
		}
	}
	return true
}
func backupTemplatePrint(newline bool, args ...any) (string, error) {
	if !backupTemplateArgs(args) {
		return "", backupErr("limit_exceeded")
	}
	var out string
	if newline {
		out = fmt.Sprintln(args...)
	} else {
		out = fmt.Sprint(args...)
	}
	if len(out) > backupmanifest.MaxFileBytes {
		return "", backupErr("limit_exceeded")
	}
	return out, nil
}
func backupTemplateEscape(escape func(string) string, args ...any) (string, error) {
	value, err := backupTemplatePrint(false, args...)
	if err != nil || len(value) > backupmanifest.MaxFileBytes/6 {
		return "", backupErr("limit_exceeded")
	}
	return escape(value), nil
}
func backupTemplatePrintf(format string, args ...any) (string, error) {
	if len(format) > 512 || !backupTemplateArgs(args) || strings.Contains(format, "*") {
		return "", backupErr("limit_exceeded")
	}
	// Reject huge width/precision/index before fmt allocates; all digit runs
	// are bounded even if embedded in a malformed format directive.
	for i := 0; i < len(format); i++ {
		if format[i] < '0' || format[i] > '9' {
			continue
		}
		j := i
		for j < len(format) && format[j] >= '0' && format[j] <= '9' {
			j++
		}
		n, err := strconv.ParseUint(format[i:j], 10, 32)
		if err != nil || n > 4096 {
			return "", backupErr("limit_exceeded")
		}
		i = j - 1
	}
	// Positional reuse can repeat a large string for each directive.
	for _, arg := range args {
		if s, ok := arg.(string); ok && len(s) > backupmanifest.MaxFileBytes/(1+len(format)) {
			return "", backupErr("limit_exceeded")
		}
	}
	out := fmt.Sprintf(format, args...)
	if len(out) > backupmanifest.MaxFileBytes {
		return "", backupErr("limit_exceeded")
	}
	return out, nil
}

func (b *backupGraphBuilder) decode(path string, data []byte) (*v1.ClientCommonConfig, []object, error) {
	if !bytes.Contains(data, []byte("{{")) {
		return decodeBackupFile(path, data)
	}
	rendered, names, err := RenderBackupTemplate(b.ctx, data, b.policy.TemplateEnv)
	if err != nil {
		return nil, nil, err
	}
	common, objects, err := decodeBackupRendered(path, rendered)
	if err != nil {
		return nil, nil, err
	}
	b.manifest.Version = backupmanifest.TemplateVersion
	b.manifest.TemplateRequirements = append(b.manifest.TemplateRequirements, backupmanifest.TemplateRequirement{From: path, Names: names, RenderedSHA256: backupmanifest.Digest(rendered)})
	return common, objects, nil
}
