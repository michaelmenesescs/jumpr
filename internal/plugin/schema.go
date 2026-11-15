// Package plugin loads declarative YAML plugins (SPEC §12) into validated
// Definitions. Loading is pure data handling: it reads files, decodes them
// strictly and reports every problem with the file, the line and the field.
// Nothing here runs a command or builds a shell line except RenderCommand,
// which quotes every substituted value through internal/shq.
package plugin

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"

	"gopkg.in/yaml.v3"

	"jumpr/internal/shq"
)

// APIVersion is the only plugin schema version this loader understands.
const APIVersion = "jumpr/v1"

// KindPlugin is the only kind a plugin file may declare.
const KindPlugin = "Plugin"

// Targets a plugin may run against (SPEC §12).
const (
	TargetHost      = "host"
	TargetK8s       = "k8s"
	TargetHTTP      = "http"
	TargetInventory = "inventory"
)

// Parse types a plugin may declare (SPEC §12). Task 116 implements them.
const (
	ParseJSON    = "json"
	ParseJQ      = "jq"
	ParseRegex   = "regex"
	ParseColumns = "columns"
	ParseKV      = "kv"
	ParseLines   = "lines"
	ParseRaw     = "raw"
)

var (
	// nameRe is the plugin and action name shape; it doubles as a file name.
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// lineRe pulls the line number out of a yaml.v3 error message.
	lineRe = regexp.MustCompile(`^line (\d+): (.*)$`)
)

// Definition is one plugin file decoded. The first block of fields is the YAML
// schema; File and Line are filled in by the loader and never decoded.
type Definition struct {
	APIVersion  string      `yaml:"apiVersion"`
	Kind        string      `yaml:"kind"`
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Target      string      `yaml:"target"`
	Privileged  bool        `yaml:"privileged"`
	Command     string      `yaml:"command"`
	Params      []ParamDef  `yaml:"params"`
	Parse       ParseDef    `yaml:"parse"`
	View        ViewDef     `yaml:"view"`
	Links       []LinkDef   `yaml:"links"`
	Actions     []ActionDef `yaml:"actions"`
	Connection  string      `yaml:"connection"`
	Request     *RequestDef `yaml:"request"`
	EmitsHosts  string      `yaml:"emits_hosts"`

	File string `yaml:"-"` // path the definition was read from
	Line int    `yaml:"-"` // line of the name field
}

// ParamDef declares one parameter a template may reference. In YAML it is
// either a bare name or a mapping with the fields below, so both
// `params: [pool]` and `params: [{name: pool, required: true}]` load.
type ParamDef struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
}

// UnmarshalYAML accepts a scalar (the parameter name) or a strict mapping.
func (p *ParamDef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		p.Name = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: params entry must be a name or a mapping", n.Line)
	}
	p.Name = ""
	p.Description = ""
	p.Required = false
	p.Default = ""
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		switch key.Value {
		case "name":
			p.Name = val.Value
		case "description":
			p.Description = val.Value
		case "required":
			p.Required = val.Value == "true"
		case "default":
			p.Default = val.Value
		default:
			return fmt.Errorf("line %d: field %s not found in params entry", key.Line, key.Value)
		}
	}
	return nil
}

// ParseDef describes how remote or HTTP output becomes rows (SPEC §12).
type ParseDef struct {
	Type      string   `yaml:"type"`      // json | jq | regex | columns | kv | lines | raw
	Pattern   string   `yaml:"pattern"`   // regex
	Expr      string   `yaml:"expr"`      // jq
	Multiline bool     `yaml:"multiline"` // regex: apply the pattern to the whole output
	Header    bool     `yaml:"header"`    // columns: first line is the header
	Names     []string `yaml:"names"`     // columns: column names
	Skip      int      `yaml:"skip"`      // columns: leading lines to drop
	Separator string   `yaml:"separator"` // kv: "=" (default) or ":"
	Strings   bool     `yaml:"strings"`   // keep every value a string
}

// ViewDef is the table shape of the result (SPEC §12).
type ViewDef struct {
	Columns       []string `yaml:"columns"`
	GroupByOutput bool     `yaml:"group_by_output"`
}

// LinkDef is one follow-up query offered on a row or on the view.
type LinkDef struct {
	Label string            `yaml:"label"`
	To    string            `yaml:"to"`
	Args  map[string]string `yaml:"args"`
}

// ActionDef is one write the plugin offers. It never runs directly: every
// action goes through the action bus (task 117), so it must ask for
// confirmation.
type ActionDef struct {
	Name       string `yaml:"name"`
	Command    string `yaml:"command"`
	Privileged bool   `yaml:"privileged"`
	Confirm    bool   `yaml:"confirm"`
}

// RequestDef is the HTTP request of a `target: http` plugin (SPEC §12).
type RequestDef struct {
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Safe    bool              `yaml:"safe"` // author asserts a non-GET read is safe
}

// Param returns the declared parameter called name.
func (d Definition) Param(name string) (ParamDef, bool) {
	for _, p := range d.Params {
		if p.Name == name {
			return p, true
		}
	}
	return ParamDef{}, false
}

// tmplPart is one piece of a parsed template: literal text, a parameter
// substitution or the host substitution.
type tmplPart struct {
	lit   string
	param string
	host  bool
}

// parseTemplate splits a plugin template into literal text and the two allowed
// substitutions, {{ .Param "name" }} and {{ .Host }}. Anything else — pipes,
// conditionals, variables, other fields — is an error: a plugin may not reach
// outside its declared parameters.
func parseTemplate(text string) ([]tmplPart, error) {
	if !strings.Contains(text, "{{") {
		return []tmplPart{{lit: text}}, nil
	}
	t, err := template.New("plugin").Parse(text)
	if err != nil {
		return nil, cleanTemplateErr(err)
	}
	if t.Tree == nil || t.Tree.Root == nil {
		return []tmplPart{{lit: text}}, nil
	}
	parts := make([]tmplPart, 0, 2*len(t.Tree.Root.Nodes))
	for _, n := range t.Tree.Root.Nodes {
		switch node := n.(type) {
		case *parse.TextNode:
			if len(node.Text) > 0 {
				parts = append(parts, tmplPart{lit: string(node.Text)})
			}
		case *parse.ActionNode:
			part, err := actionPart(node)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			return nil, fmt.Errorf("unsupported template construct %q: only {{ .Param \"name\" }} and {{ .Host }} are allowed", n.String())
		}
	}
	return parts, nil
}

// actionPart checks one {{ … }} action and returns the substitution it makes.
func actionPart(n *parse.ActionNode) (tmplPart, error) {
	bad := fmt.Errorf("unsupported template action %q: only {{ .Param \"name\" }} and {{ .Host }} are allowed", n.String())
	if n.Pipe == nil || len(n.Pipe.Decl) > 0 || len(n.Pipe.Cmds) != 1 {
		return tmplPart{}, bad
	}
	cmd := n.Pipe.Cmds[0]
	if len(cmd.Args) == 0 {
		return tmplPart{}, bad
	}
	field, ok := cmd.Args[0].(*parse.FieldNode)
	if !ok || len(field.Ident) != 1 {
		return tmplPart{}, bad
	}
	switch field.Ident[0] {
	case "Host":
		if len(cmd.Args) != 1 {
			return tmplPart{}, bad
		}
		return tmplPart{host: true}, nil
	case "Param":
		if len(cmd.Args) != 2 {
			return tmplPart{}, bad
		}
		arg, ok := cmd.Args[1].(*parse.StringNode)
		if !ok || arg.Text == "" {
			return tmplPart{}, errors.New("{{ .Param }} needs a quoted, non-empty parameter name")
		}
		return tmplPart{param: arg.Text}, nil
	default:
		return tmplPart{}, bad
	}
}

// cleanTemplateErr strips text/template's "template: plugin:" prefix from
// syntax errors so the message reads as a field problem.
func cleanTemplateErr(err error) error {
	msg := err.Error()
	if i := len("template: plugin:"); len(msg) > i && msg[:i] == "template: plugin:" {
		msg = strings.TrimSpace(msg[i:])
	}
	return errors.New("template does not parse: " + msg)
}

// refs returns the parameter names a parsed template references, in order.
func refs(parts []tmplPart) []string {
	var out []string
	for _, p := range parts {
		if p.param != "" {
			out = append(out, p.param)
		}
	}
	return out
}

// usesHost reports whether the template substitutes the host.
func usesHost(parts []tmplPart) bool {
	for _, p := range parts {
		if p.host {
			return true
		}
	}
	return false
}

// RenderCommand expands a command template for one target. Every substituted
// value is quoted by internal/shq, so a parameter value of `a; rm -rf /` becomes
// a single argument and never a second command.
func RenderCommand(text string, params map[string]string, host string) (string, error) {
	parts, err := parseTemplate(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		switch {
		case p.param != "":
			v, ok := params[p.param]
			if !ok {
				return "", fmt.Errorf("parameter %q has no value", p.param)
			}
			b.WriteString(shq.Quote(v))
		case p.host:
			b.WriteString(shq.Quote(host))
		default:
			b.WriteString(p.lit)
		}
	}
	return b.String(), nil
}
