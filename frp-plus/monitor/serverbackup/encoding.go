package serverbackup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

func relocationEncode(path string, raw map[string]any) ([]byte, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return json.MarshalIndent(raw, "", "  ")
	case ".toml":
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var out strings.Builder
		for _, key := range keys {
			name, _ := json.Marshal(key)
			value, err := relocationTOML(raw[key])
			if err != nil {
				return nil, err
			}
			out.Write(name)
			out.WriteString(" = ")
			out.WriteString(value)
			out.WriteByte('\n')
		}
		return []byte(out.String()), nil
	case ".yaml", ".yml":
		node, err := relocationYAML(raw)
		if err != nil {
			return nil, err
		}
		return yaml.Marshal(node)
	}
	return nil, failure("unsupported_format")
}
func relocationYAML(value any) (*yaml.Node, error) {
	switch v := value.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, err := relocationYAML(v[key])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, Style: yaml.DoubleQuotedStyle}, child)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range v {
			child, err := relocationYAML(item)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, child)
		}
		return n, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(string(v), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(v)}, nil
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	default:
		return nil, failure("source_invalid")
	}
}

func relocationTOML(value any) (string, error) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, key := range keys {
			name, _ := json.Marshal(key)
			text, err := relocationTOML(v[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, string(name)+" = "+text)
		}
		return "{ " + strings.Join(parts, ", ") + " }", nil
	case []any:
		parts := []string{}
		for _, item := range v {
			text, err := relocationTOML(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case json.Number, string, bool:
		data, err := json.Marshal(v)
		if err != nil {
			return "", failure("source_invalid")
		}
		return string(data), nil
	default:
		return "", failure("source_invalid")
	}
}

const relocationTemplatePrefix = "FRPPLUSBACKUPTEMPLATE"

type relocationAction struct {
	marker string
	raw    []byte
	quoted bool
}

// Symbolization preserves exact scalar template expressions, not rendered
// values. Control-flow templates can be kept byte-for-byte by the fast path;
// rewriting them is explicitly unsupported instead of guessing their syntax.
func relocationSymbolize(path string, data []byte) ([]byte, []relocationAction, error) {
	if bytes.Contains(data, []byte(relocationTemplatePrefix)) {
		return nil, nil, failure("relocation_template_unsupported")
	}
	var out bytes.Buffer
	actions := []relocationAction{}
	quote := byte(0)
	triple := false
	comment := false
	format := strings.ToLower(filepath.Ext(path))
	for i := 0; i < len(data); {
		if i+1 < len(data) && data[i] == '{' && data[i+1] == '{' {
			end, err := relocationActionEnd(data, i+2)
			if err != nil {
				return nil, nil, err
			}
			raw := data[i:end]
			noop := func(...any) any { return nil }
			parsed, err := template.New("scalar").Funcs(template.FuncMap{"parseNumberRange": noop, "parseNumberRangePair": noop}).Parse(string(raw))
			if err != nil || len(parsed.Tree.Root.Nodes) != 1 {
				return nil, nil, failure("relocation_template_unsupported")
			}
			node, ok := parsed.Tree.Root.Nodes[0].(*parse.ActionNode)
			if !ok || len(node.Pipe.Decl) != 0 {
				return nil, nil, failure("relocation_template_unsupported")
			}
			marker := fmt.Sprintf("%s%06dZ", relocationTemplatePrefix, len(actions)+1)
			inside := quote != 0
			if !inside && format != ".yaml" && format != ".yml" {
				out.WriteByte('"')
			}
			out.WriteString(marker)
			if !inside && format != ".yaml" && format != ".yml" {
				out.WriteByte('"')
			}
			actions = append(actions, relocationAction{marker, append([]byte(nil), raw...), inside})
			i = end
			continue
		}
		ch := data[i]
		if ch == '\n' {
			comment = false
		}
		if !comment {
			if quote != 0 {
				if ch == '\\' && quote == '"' && i+1 < len(data) {
					out.Write(data[i : i+2])
					i += 2
					continue
				}
				if ch == quote {
					if triple {
						if i+2 < len(data) && data[i+1] == quote && data[i+2] == quote {
							out.Write(data[i : i+3])
							i += 3
							quote = 0
							triple = false
							continue
						}
					} else {
						quote = 0
					}
				}
			} else if ch == '#' && format != ".json" {
				comment = true
			} else if ch == '"' || ch == '\'' && format != ".json" {
				quote = ch
				if format == ".toml" && i+2 < len(data) && data[i+1] == ch && data[i+2] == ch {
					triple = true
					out.Write(data[i : i+3])
					i += 3
					continue
				}
			}
		}
		out.WriteByte(ch)
		i++
	}
	return out.Bytes(), actions, nil
}
func relocationActionEnd(data []byte, start int) (int, error) {
	quote := byte(0)
	for i := start; i < len(data); i++ {
		ch := data[i]
		if quote != 0 {
			if ch == '\\' && quote != '`' {
				i++
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			quote = ch
			continue
		}
		if ch == '}' && i+1 < len(data) && data[i+1] == '}' {
			return i + 2, nil
		}
	}
	return 0, failure("relocation_template_unsupported")
}
