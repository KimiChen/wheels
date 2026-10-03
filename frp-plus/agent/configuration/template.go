package configuration

import (
	"text/template"
	"text/template/parse"
)

// Only provably named environment reads are managed. In particular, iterating
// or printing the whole environment would make restart recovery depend on
// unrelated service-manager variables and might expose their values.
func (s *snapshot) templateDependencies(data []byte) error {
	return walkTemplateDependencies(data, func(key string) error {
		value, ok := s.in.TemplateEnv[key]
		if !ok {
			return failure("unsupported_dependency")
		}
		s.referencedEnv[key] = value
		return nil
	})
}

// walkTemplateDependencies inspects every definition, including branches that
// did not execute. It never reads the environment or returns its values.
func walkTemplateDependencies(data []byte, use func(string) error) error {
	noop := func(...any) any { return nil }
	t, err := template.New("frp").Funcs(template.FuncMap{"parseNumberRange": noop, "parseNumberRangePair": noop}).Parse(string(data))
	if err != nil {
		return failure("invalid_template")
	}
	var walk func(parse.Node) error
	bareEnv := func(node parse.Node) bool {
		switch n := node.(type) {
		case *parse.FieldNode:
			return len(n.Ident) == 1 && n.Ident[0] == "Envs"
		case *parse.VariableNode:
			return len(n.Ident) == 2 && n.Ident[0] == "$" && n.Ident[1] == "Envs"
		}
		return false
	}
	walk = func(node parse.Node) error {
		if node == nil {
			return nil
		}
		switch n := node.(type) {
		case *parse.ListNode:
			if n == nil {
				return nil
			}
			for _, item := range n.Nodes {
				if err := walk(item); err != nil {
					return err
				}
			}
		case *parse.ActionNode:
			return walk(n.Pipe)
		case *parse.PipeNode:
			if n == nil {
				return nil
			}
			for _, command := range n.Cmds {
				if err := walk(command); err != nil {
					return err
				}
			}
		case *parse.CommandNode:
			if len(n.Args) > 1 {
				if id, ok := n.Args[0].(*parse.IdentifierNode); ok && id.Ident == "index" && bareEnv(n.Args[1]) {
					if len(n.Args) != 3 {
						return failure("unsupported_dependency")
					}
					key, ok := n.Args[2].(*parse.StringNode)
					if !ok {
						return failure("unsupported_dependency")
					}
					return use(key.Text)
				}
			}
			for _, arg := range n.Args {
				if err := walk(arg); err != nil {
					return err
				}
			}
		case *parse.FieldNode:
			if len(n.Ident) == 2 && n.Ident[0] == "Envs" {
				return use(n.Ident[1])
			}
			return failure("unsupported_dependency")
		case *parse.VariableNode:
			if len(n.Ident) == 3 && n.Ident[0] == "$" && n.Ident[1] == "Envs" {
				return use(n.Ident[2])
			}
			// Named scalar variables are safe only because assigning a root/env
			// object is rejected while walking the defining pipeline.
			if len(n.Ident) == 1 && n.Ident[0] != "$" {
				return nil
			}
			return failure("unsupported_dependency")
		case *parse.DotNode, *parse.ChainNode:
			return failure("unsupported_dependency")
		case *parse.IfNode:
			if err := walk(n.Pipe); err != nil {
				return err
			}
			if err := walk(n.List); err != nil {
				return err
			}
			return walk(n.ElseList)
		case *parse.RangeNode:
			if err := walk(n.Pipe); err != nil {
				return err
			}
			if err := walk(n.List); err != nil {
				return err
			}
			return walk(n.ElseList)
		case *parse.WithNode:
			if err := walk(n.Pipe); err != nil {
				return err
			}
			if err := walk(n.List); err != nil {
				return err
			}
			return walk(n.ElseList)
		case *parse.TemplateNode:
			return walk(n.Pipe)
		case *parse.TextNode, *parse.StringNode, *parse.NumberNode, *parse.BoolNode, *parse.NilNode, *parse.IdentifierNode, *parse.CommentNode, *parse.BreakNode, *parse.ContinueNode:
		default:
			return failure("unsupported_dependency")
		}
		return nil
	}
	for _, defined := range t.Templates() {
		if defined.Tree != nil {
			if err := walk(defined.Tree.Root); err != nil {
				return err
			}
		}
	}
	return nil
}
