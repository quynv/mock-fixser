package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/quynv/mock-fixser/schema"
)

// FieldError is one validation problem, located by a path such as
// symbols[0].max and, when known, the YAML line number.
type FieldError struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

func (e FieldError) String() string {
	loc := e.Path
	if loc == "" {
		loc = "(root)"
	}
	if e.Line > 0 {
		loc = fmt.Sprintf("line %d: %s", e.Line, loc)
	}
	return loc + ": " + e.Message
}

// ValidationError lists every problem found in a config.
type ValidationError struct {
	Errors []FieldError
}

// newValidationError sorts errors by line so they read top to bottom.
func newValidationError(errs []FieldError) *ValidationError {
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Line < errs[j].Line })
	return &ValidationError{Errors: errs}
}

func (e *ValidationError) Error() string {
	lines := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		lines[i] = fe.String()
	}
	return "invalid config:\n  " + strings.Join(lines, "\n  ")
}

// Parse decodes and validates a YAML config. Validation runs the JSON
// Schema first, then the cross-field checks of section 9.2. Defaults are
// applied before the cross-field checks. Errors are a *ValidationError.
func Parse(data []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, &ValidationError{Errors: []FieldError{yamlSyntaxError(err)}}
	}
	if len(root.Content) == 0 {
		return nil, &ValidationError{Errors: []FieldError{{Message: "config is empty"}}}
	}
	doc := root.Content[0]

	if errs := validateSchema(doc); len(errs) > 0 {
		return nil, newValidationError(errs)
	}

	cfg := &Config{}
	if err := doc.Decode(cfg); err != nil {
		return nil, &ValidationError{Errors: []FieldError{yamlSyntaxError(err)}}
	}
	applyDefaults(cfg)

	if errs := validate(cfg); len(errs) > 0 {
		for i := range errs {
			errs[i].Line = lineOf(doc, errs[i].Path)
		}
		return nil, newValidationError(errs)
	}
	return cfg, nil
}

// Load reads and parses a config file.
func Load(path string) (*Config, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := Parse(data)
	return cfg, data, err
}

func yamlSyntaxError(err error) FieldError {
	msg := err.Error()
	fe := FieldError{Message: strings.TrimPrefix(msg, "yaml: ")}
	// yaml.v3 reports "yaml: line N: ..." for syntax errors.
	if rest, ok := strings.CutPrefix(fe.Message, "line "); ok {
		if n, tail, ok := strings.Cut(rest, ": "); ok {
			if line, err := strconv.Atoi(n); err == nil {
				fe.Line, fe.Message = line, tail
			}
		}
	}
	return fe
}

var printer = message.NewPrinter(language.English)

var (
	schemaOnce     sync.Once
	compiledSchema *jsonschema.Schema
	errSchema      error
)

func configSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.ConfigJSON))
		if err != nil {
			errSchema = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("config.schema.json", doc); err != nil {
			errSchema = err
			return
		}
		compiledSchema, errSchema = c.Compile("config.schema.json")
	})
	return compiledSchema, errSchema
}

func validateSchema(doc *yaml.Node) []FieldError {
	sch, err := configSchema()
	if err != nil {
		return []FieldError{{Message: "schema: " + err.Error()}}
	}

	// Round-trip through JSON so the validator sees JSON types only.
	var v any
	if err := doc.Decode(&v); err != nil {
		return []FieldError{yamlSyntaxError(err)}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return []FieldError{{Message: err.Error()}}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return []FieldError{{Message: err.Error()}}
	}

	err = sch.Validate(inst)
	var verr *jsonschema.ValidationError
	if err == nil {
		return nil
	}
	if !errors.As(err, &verr) {
		return []FieldError{{Message: err.Error()}}
	}

	var out []FieldError
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		path := locationToPath(e.InstanceLocation)
		msg := e.ErrorKind.LocalizedString(printer)
		if seen[path+msg] {
			return
		}
		seen[path+msg] = true
		out = append(out, FieldError{Path: path, Line: lineOf(doc, path), Message: msg})
	}
	walk(verr)
	return out
}

// locationToPath turns ["symbols", "0", "max"] into symbols[0].max.
func locationToPath(loc []string) string {
	var b strings.Builder
	for _, tok := range loc {
		if _, err := strconv.Atoi(tok); err == nil {
			b.WriteByte('[')
			b.WriteString(tok)
			b.WriteByte(']')
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(tok)
	}
	return b.String()
}

// lineOf returns the YAML line of the node at path, or of its closest
// existing ancestor, so a missing field points at its parent.
func lineOf(doc *yaml.Node, path string) int {
	n := doc
	line := doc.Line
	for _, tok := range splitPath(path) {
		var next *yaml.Node
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == tok {
					// Point at the key so the editor jumps to the field name.
					line = n.Content[i].Line
					next = n.Content[i+1]
					break
				}
			}
		case yaml.SequenceNode:
			if idx, err := strconv.Atoi(tok); err == nil && idx < len(n.Content) {
				next = n.Content[idx]
				line = next.Line
			}
		}
		if next == nil {
			return line
		}
		n = next
	}
	return line
}

func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	path = strings.NewReplacer("[", ".", "]", "").Replace(path)
	return strings.Split(strings.TrimPrefix(path, "."), ".")
}

func applyDefaults(c *Config) {
	fix := &c.Server.FIX
	setDefault(&fix.Listen, "0.0.0.0:9876")
	setDefault(&fix.Version, "FIX.4.4")
	setDefault(&fix.DataDictionary, "spec/FIX44.xml")

	mon := &c.Server.Monitor
	setDefault(&mon.Listen, "127.0.0.1:9877")
	setDefault(&mon.SenderCompID, "MOCKSRV")
	setDefault(&mon.TargetCompID, "UI_MONITOR")
	if mon.MirrorBuffer == 0 {
		mon.MirrorBuffer = 10000
	}

	h := &c.Server.HTTP
	setDefault(&h.Listen, "0.0.0.0:8080")
	if h.HistoryTicks == 0 {
		h.HistoryTicks = 5000
	}

	b := &c.MarketData.Batching
	setDefault(&b.Scope, "per_request")
	setDefault(&b.InitialSnapshot, "per_symbol_w")
	if b.FlushIntervalMs == 0 {
		b.FlushIntervalMs = 100
	}
	if b.MaxSymbolsPerMsg == 0 {
		b.MaxSymbolsPerMsg = 20
	}
	if b.MaxEntriesPerMsg == 0 {
		b.MaxEntriesPerMsg = 200
	}

	for i := range c.Symbols {
		s := &c.Symbols[i]
		setDefault(&s.Model, "random_walk")
		setDefault(&s.UpdateMode, "incremental")
		if s.DepthLevels == 0 {
			s.DepthLevels = 1
		}
	}
}

func setDefault(field *string, v string) {
	if *field == "" {
		*field = v
	}
}
