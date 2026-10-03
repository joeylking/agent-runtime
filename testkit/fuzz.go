package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	agentrt "github.com/joeylking/agent-runtime"
)

// Fuzz generates tool arguments from a tool's JSON Schema, with the same
// schema library the runtime validates with. Generation is deterministic
// for a Seed and bounded in size. It covers the subset the consumers'
// schemas use: type (one or several), enum, const, object properties with
// required, additionalProperties, minProperties and maxProperties, array
// items with minItems, maxItems, uniqueItems and prefixItems, string
// minLength and maxLength, numeric minimum and maximum (exclusive too),
// anyOf and oneOf by picking a branch, and $ref within the schema. A
// schema using pattern, multipleOf, allOf, not, if, patternProperties,
// propertyNames, dependencies, contains, or the unevaluated keywords is
// reported as one the kit cannot generate for.
type Fuzz struct {
	// Seed selects the sequence. Zero means 1.
	Seed uint64
	// Count is how many arguments Valid returns and at most how many
	// Invalid returns. Zero means 32.
	Count int
	// MaxItems bounds a generated array where the schema does not, and
	// MaxLength a generated string, in characters. Zero means 4 and 32.
	MaxItems  int
	MaxLength int
	// MaxDepth bounds nesting where the schema recurses: below it, optional
	// properties are left out and arrays are as short as allowed. Zero
	// means 4.
	MaxDepth int
}

func (f Fuzz) seed() uint64 {
	if f.Seed == 0 {
		return 1
	}
	return f.Seed
}

func (f Fuzz) count() int {
	if f.Count <= 0 {
		return 32
	}
	return f.Count
}

func (f Fuzz) maxItems() int {
	if f.MaxItems <= 0 {
		return 4
	}
	return f.MaxItems
}

func (f Fuzz) maxLength() int {
	if f.MaxLength <= 0 {
		return 32
	}
	return f.MaxLength
}

func (f Fuzz) maxDepth() int {
	if f.MaxDepth <= 0 {
		return 4
	}
	return f.MaxDepth
}

// Valid returns Count arguments that satisfy schema. Each is checked
// against the compiled schema before it is returned.
func (f Fuzz) Valid(schema json.RawMessage) ([]json.RawMessage, error) {
	s, err := compile(schema)
	if err != nil {
		return nil, err
	}
	g := &gen{r: rand.New(rand.NewPCG(f.seed(), 0x9e3779b97f4a7c15)), f: f}
	out := make([]json.RawMessage, 0, f.count())
	for len(out) < f.count() {
		b, err := g.valid(s)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// Invalid returns up to Count near misses: valid arguments with one change
// the schema refuses (a required property missing, an unexpected one
// present, a wrong type, an enum outsider, a length, bound, or item count
// just past its limit, a non-object root), each confirmed invalid against
// the compiled schema, after the cases the runtime refuses at its boundary
// before any schema: nesting past its depth cap, a duplicate object key,
// invalid UTF-8, an unpaired surrogate escape, truncated JSON, and two
// values. A schema with nothing to violate yields the boundary cases only.
func (f Fuzz) Invalid(schema json.RawMessage) ([]json.RawMessage, error) {
	s, err := compile(schema)
	if err != nil {
		return nil, err
	}
	g := &gen{r: rand.New(rand.NewPCG(f.seed()^0x5bd1e995, 0x9e3779b97f4a7c15)), f: f}
	first, err := g.valid(s)
	if err != nil {
		return nil, err
	}
	out := boundary(first, s)
	seen := map[string]bool{}
	for _, b := range out {
		seen[string(b)] = true
	}
	// Mutations are drawn from successive valid values until Count is
	// reached or a round adds nothing new.
	for len(out) < f.count() {
		b, err := g.valid(s)
		if err != nil {
			return nil, err
		}
		added := false
		ms := g.mutations(s, b)
		// Shuffled, so a small Count still samples every kind of change
		// rather than the first property's alone.
		g.r.Shuffle(len(ms), func(i, j int) { ms[i], ms[j] = ms[j], ms[i] })
		for _, m := range ms {
			if seen[string(m)] || len(out) >= f.count() {
				continue
			}
			seen[string(m)] = true
			out = append(out, m)
			added = true
		}
		if !added {
			break
		}
	}
	return out, nil
}

// Check puts every tool's arguments through the runtime's acceptance of a
// tool call (agentrt.CompileTool and its Check): the valid ones must pass
// it, then reach the tool and return without a panic; the invalid ones
// must be refused, as the driver refuses them with an invalid_decision,
// and never reach the tool. A tool's own error on a valid argument is
// allowed, since a schema does not say what a tool accepts. Each call is
// bounded by the tool's Timeout, as the driver bounds it; a Terminal tool
// is called like any other.
func (f Fuzz) Check(t testing.TB, tools ...agentrt.Tool) {
	t.Helper()
	for _, tool := range tools {
		f.checkTool(t, tool)
	}
}

func (f Fuzz) checkTool(t testing.TB, tool agentrt.Tool) {
	t.Helper()
	spec := tool.Spec()
	ts, err := agentrt.CompileTool(spec)
	if err != nil {
		t.Fatalf("testkit: %s: %v", spec.Name, err)
	}
	valid, err := f.Valid(spec.InputSchema)
	if err != nil {
		t.Errorf("testkit: %s: %v", spec.Name, err)
		return
	}
	invalid, err := f.Invalid(spec.InputSchema)
	if err != nil {
		t.Errorf("testkit: %s: %v", spec.Name, err)
		return
	}
	for i, a := range valid {
		args := clip(string(a), 200)
		if err := ts.Check(a); err != nil {
			t.Errorf("testkit: %s refused a valid argument %s: invalid decision: %v", spec.Name, args, err)
			continue
		}
		if p, panicked := call(tool, spec, fmt.Sprintf("testkit-fuzz-%d", i), a); panicked {
			t.Errorf("testkit: %s panicked on %s: %v", spec.Name, args, p)
		}
	}
	for _, a := range invalid {
		if ts.Check(a) == nil {
			t.Errorf("testkit: %s accepted an invalid argument %s", spec.Name, clip(string(a), 200))
		}
	}
}

// call calls tool with args under its timeout and reports a panic; the
// tool's result and error are not judged.
func call(tool agentrt.Tool, spec agentrt.ToolSpec, stepID string, args json.RawMessage) (p any, panicked bool) {
	ctx, cancel := context.WithTimeout(context.Background(), spec.Timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			p, panicked = r, true
		}
	}()
	tool.Call(ctx, agentrt.ToolCall{RunID: "testkit-fuzz", StepID: stepID, Args: args})
	return nil, false
}

// compile compiles a tool's schema as the runtime does: from itself and
// the embedded metaschemas, loading no URL.
func compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("input schema is required")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("input schema is not valid JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	const url = "testkit://schema.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("input schema does not compile: %w", err)
	}
	return s, nil
}

// accepts reports whether the compiled schema accepts raw.
func accepts(s *jsonschema.Schema, raw []byte) bool {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	return s.Validate(v) == nil
}

type gen struct {
	r *rand.Rand
	f Fuzz
}

// valid generates one value and confirms the schema accepts it.
func (g *gen) valid(s *jsonschema.Schema) (json.RawMessage, error) {
	v, err := g.value(s, 0)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if !accepts(s, b) {
		return nil, fmt.Errorf("%s: the kit generated %s, which the schema refuses; the schema combines keywords the kit does not model", s.Location, clip(string(b), 200))
	}
	return b, nil
}

// resolve follows $ref to the schema that carries the keywords.
func resolve(s *jsonschema.Schema) *jsonschema.Schema {
	for s.Ref != nil {
		s = s.Ref
	}
	return s
}

func unsupported(s *jsonschema.Schema) error {
	var used []string
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"pattern", s.Pattern != nil}, {"multipleOf", s.MultipleOf != nil}, {"allOf", len(s.AllOf) > 0}, {"not", s.Not != nil},
		{"if", s.If != nil}, {"patternProperties", len(s.PatternProperties) > 0}, {"propertyNames", s.PropertyNames != nil},
		{"dependencies", len(s.Dependencies) > 0}, {"dependentRequired", len(s.DependentRequired) > 0}, {"dependentSchemas", len(s.DependentSchemas) > 0},
		{"contains", s.Contains != nil}, {"unevaluatedProperties", s.UnevaluatedProperties != nil}, {"unevaluatedItems", s.UnevaluatedItems != nil},
		{"$dynamicRef", s.DynamicRef != nil}, {"$recursiveRef", s.RecursiveRef != nil},
	} {
		if k.set {
			used = append(used, k.name)
		}
	}
	if len(used) == 0 {
		return nil
	}
	return fmt.Errorf("%s uses %s, which the kit does not generate for", s.Location, strings.Join(used, ", "))
}

func (g *gen) value(s *jsonschema.Schema, depth int) (any, error) {
	s = resolve(s)
	if s.Bool != nil {
		if !*s.Bool {
			return nil, fmt.Errorf("%s admits nothing", s.Location)
		}
		return map[string]any{}, nil
	}
	if err := unsupported(s); err != nil {
		return nil, err
	}
	if s.Const != nil {
		return *s.Const, nil
	}
	if s.Enum != nil && len(s.Enum.Values) > 0 {
		return s.Enum.Values[g.r.IntN(len(s.Enum.Values))], nil
	}
	if n := len(s.AnyOf); n > 0 {
		return g.value(s.AnyOf[g.r.IntN(n)], depth)
	}
	if n := len(s.OneOf); n > 0 {
		return g.value(s.OneOf[g.r.IntN(n)], depth)
	}
	types := typesOf(s)
	switch types[g.r.IntN(len(types))] {
	case "object":
		return g.object(s, depth)
	case "array":
		return g.array(s, depth)
	case "string":
		return g.str(s), nil
	case "integer":
		return g.integer(s), nil
	case "number":
		return g.number(s), nil
	case "boolean":
		return g.r.IntN(2) == 0, nil
	default:
		return nil, nil
	}
}

// typesOf is the schema's types, or the one its keywords imply.
func typesOf(s *jsonschema.Schema) []string {
	if s.Types != nil {
		if t := s.Types.ToStrings(); len(t) > 0 {
			return t
		}
	}
	switch {
	case len(s.Properties) > 0 || len(s.Required) > 0 || s.AdditionalProperties != nil || s.MinProperties != nil || s.MaxProperties != nil:
		return []string{"object"}
	case s.Items2020 != nil || s.Items != nil || len(s.PrefixItems) > 0 || s.MinItems != nil || s.MaxItems != nil:
		return []string{"array"}
	case s.Minimum != nil || s.Maximum != nil || s.ExclusiveMinimum != nil || s.ExclusiveMaximum != nil:
		return []string{"number"}
	}
	return []string{"string"}
}

func (g *gen) object(s *jsonschema.Schema, depth int) (any, error) {
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	required := map[string]bool{}
	for _, k := range s.Required {
		required[k] = true
	}
	m := map[string]any{}
	var optional []string
	for _, k := range keys {
		if required[k] {
			v, err := g.value(s.Properties[k], depth+1)
			if err != nil {
				return nil, err
			}
			m[k] = v
			continue
		}
		optional = append(optional, k)
	}
	// A required property without a schema of its own takes whatever
	// additionalProperties allows.
	for _, k := range s.Required {
		if _, ok := s.Properties[k]; ok {
			continue
		}
		v, err := g.additional(s, depth)
		if err != nil {
			return nil, fmt.Errorf("%s requires %q, which no schema admits", s.Location, k)
		}
		m[k] = v
	}
	max := math.MaxInt
	if s.MaxProperties != nil {
		max = *s.MaxProperties
	}
	for _, k := range optional {
		if len(m) >= max || depth >= g.f.maxDepth() || g.r.IntN(2) == 0 {
			continue
		}
		v, err := g.value(s.Properties[k], depth+1)
		if err != nil {
			return nil, err
		}
		m[k] = v
	}
	if s.MinProperties != nil {
		for _, k := range optional {
			if len(m) >= *s.MinProperties {
				break
			}
			if _, ok := m[k]; ok {
				continue
			}
			v, err := g.value(s.Properties[k], depth+1)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		for i := 0; len(m) < *s.MinProperties; i++ {
			v, err := g.additional(s, depth)
			if err != nil {
				return nil, fmt.Errorf("%s needs %d properties but admits fewer", s.Location, *s.MinProperties)
			}
			m[fmt.Sprintf("extra_%d", i)] = v
		}
	}
	return m, nil
}

// additional generates a value additionalProperties admits, or fails when
// it admits none.
func (g *gen) additional(s *jsonschema.Schema, depth int) (any, error) {
	switch ap := s.AdditionalProperties.(type) {
	case bool:
		if !ap {
			return nil, errors.New("additionalProperties is false")
		}
	case *jsonschema.Schema:
		return g.value(ap, depth+1)
	}
	return g.str(nil), nil
}

func (g *gen) array(s *jsonschema.Schema, depth int) (any, error) {
	lo, hi := 0, g.f.maxItems()
	if s.MinItems != nil {
		lo = *s.MinItems
	}
	if s.MaxItems != nil && *s.MaxItems < hi {
		hi = *s.MaxItems
	}
	if hi < lo {
		hi = lo
	}
	if depth >= g.f.maxDepth() {
		hi = lo
	}
	n := lo + g.r.IntN(hi-lo+1)
	item := itemSchema(s)
	out := make([]any, 0, n)
	seen := map[string]bool{}
	for i := 0; len(out) < n; i++ {
		is := item
		if i < len(s.PrefixItems) {
			is = s.PrefixItems[i]
		}
		if is == nil {
			if i < len(s.PrefixItems) || len(s.PrefixItems) == 0 {
				out = append(out, g.str(nil))
				continue
			}
			break // items: false after the prefix
		}
		v, err := g.value(is, depth+1)
		if err != nil {
			return nil, err
		}
		if s.UniqueItems {
			b, _ := json.Marshal(v)
			if seen[string(b)] {
				if i > 10*n+10 {
					return nil, fmt.Errorf("%s: cannot make %d unique items", s.Location, n)
				}
				continue
			}
			seen[string(b)] = true
		}
		out = append(out, v)
	}
	return out, nil
}

// itemSchema is the schema of an array's items, or nil for none.
func itemSchema(s *jsonschema.Schema) *jsonschema.Schema {
	if s.Items2020 != nil {
		return s.Items2020
	}
	if is, ok := s.Items.(*jsonschema.Schema); ok {
		return is
	}
	return nil
}

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 _-./:<>&\"\\éß→"

func (g *gen) str(s *jsonschema.Schema) string {
	lo, hi := 0, g.f.maxLength()
	if s != nil {
		if s.MinLength != nil {
			lo = *s.MinLength
		}
		if s.MaxLength != nil && *s.MaxLength < hi {
			hi = *s.MaxLength
		}
	}
	if hi < lo {
		hi = lo
	}
	n := lo + g.r.IntN(hi-lo+1)
	runes := []rune(alphabet)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(runes[g.r.IntN(len(runes))])
	}
	return b.String()
}

// bounds is the inclusive integer range a numeric schema allows, within
// the kit's default of -1000 to 1000 when the schema is wider.
func bounds(s *jsonschema.Schema) (lo, hi int64) {
	lo, hi = -1000, 1000
	if s.Minimum != nil {
		f, _ := s.Minimum.Float64()
		lo = int64(math.Ceil(f))
	}
	if s.ExclusiveMinimum != nil {
		f, _ := s.ExclusiveMinimum.Float64()
		lo = int64(math.Floor(f)) + 1
	}
	if s.Maximum != nil {
		f, _ := s.Maximum.Float64()
		hi = int64(math.Floor(f))
	}
	if s.ExclusiveMaximum != nil {
		f, _ := s.ExclusiveMaximum.Float64()
		hi = int64(math.Ceil(f)) - 1
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

func (g *gen) integer(s *jsonschema.Schema) json.Number {
	lo, hi := bounds(s)
	return json.Number(strconv.FormatInt(lo+g.r.Int64N(hi-lo+1), 10))
}

func (g *gen) number(s *jsonschema.Schema) json.Number {
	lo, hi := bounds(s)
	v := float64(lo) + g.r.Float64()*float64(hi-lo)
	// Two decimals, kept inside the bounds the schema states.
	v = math.Round(v*100) / 100
	if v < float64(lo) {
		v = float64(lo)
	}
	if v > float64(hi) {
		v = float64(hi)
	}
	return json.Number(strconv.FormatFloat(v, 'f', -1, 64))
}

// mutations returns the near misses of a valid value: each is one change
// the schema refuses, confirmed against it.
func (g *gen) mutations(s *jsonschema.Schema, valid json.RawMessage) []json.RawMessage {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(valid))
	if err != nil {
		return nil
	}
	var out []json.RawMessage
	for _, c := range g.mutate(resolve(s), v) {
		b, err := json.Marshal(c)
		if err != nil || accepts(s, b) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// mutate lists candidate near misses of v under s; the caller keeps the
// ones the schema refuses.
func (g *gen) mutate(s *jsonschema.Schema, v any) []any {
	var out []any
	// The wrong types, each a value the schema would have to refuse by
	// type alone.
	out = append(out, "x", json.Number("1"), true, nil, []any{}, map[string]any{})
	if s.Enum != nil {
		out = append(out, "not-a-member-"+strconv.Itoa(g.r.IntN(1000)))
	}
	if s.Const != nil {
		out = append(out, "not-the-constant")
	}
	switch x := v.(type) {
	case string:
		if s.MaxLength != nil {
			out = append(out, strings.Repeat("a", *s.MaxLength+1))
		}
		if s.MinLength != nil && *s.MinLength > 0 {
			out = append(out, strings.Repeat("a", *s.MinLength-1))
		}
	case json.Number:
		lo, hi := bounds(s)
		if s.Maximum != nil || s.ExclusiveMaximum != nil {
			out = append(out, json.Number(strconv.FormatInt(hi+1, 10)))
		}
		if s.Minimum != nil || s.ExclusiveMinimum != nil {
			out = append(out, json.Number(strconv.FormatInt(lo-1, 10)))
		}
		out = append(out, json.Number("1.5"), json.Number("1e400"))
	case []any:
		if s.MaxItems != nil {
			over := make([]any, *s.MaxItems+1)
			for i := range over {
				if len(x) > 0 {
					over[i] = x[i%len(x)]
				} else {
					over[i] = "x"
				}
			}
			out = append(out, over)
		}
		if s.MinItems != nil && *s.MinItems > 0 {
			out = append(out, x[:*s.MinItems-1])
		}
		if is := itemSchema(s); is != nil && len(x) > 0 {
			for _, m := range g.mutate(resolve(is), x[0]) {
				c := append([]any(nil), x...)
				c[0] = m
				out = append(out, c)
			}
		}
	case map[string]any:
		for _, k := range s.Required {
			c := clone(x)
			delete(c, k)
			out = append(out, c)
		}
		if ap, ok := s.AdditionalProperties.(bool); ok && !ap {
			c := clone(x)
			c["unexpected"] = json.Number("1")
			out = append(out, c)
		}
		if s.MaxProperties != nil {
			c := clone(x)
			for i := 0; len(c) <= *s.MaxProperties; i++ {
				c[fmt.Sprintf("over_%d", i)] = json.Number("1")
			}
			out = append(out, c)
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ps, ok := s.Properties[k]
			if !ok {
				continue
			}
			for _, m := range g.mutate(resolve(ps), x[k]) {
				c := clone(x)
				c[k] = m
				out = append(out, c)
			}
		}
	}
	return out
}

func clone(m map[string]any) map[string]any {
	c := make(map[string]any, len(m)+1)
	for k, v := range m {
		c[k] = v
	}
	return c
}

// boundary builds the arguments the runtime refuses before any schema,
// from a valid value where that makes the boundary the only refuser.
func boundary(valid json.RawMessage, s *jsonschema.Schema) []json.RawMessage {
	deep := []byte(`{"_":`)
	for i := 0; i < 300; i++ {
		deep = append(deep, '[')
	}
	for i := 0; i < 300; i++ {
		deep = append(deep, ']')
	}
	deep = append(deep, '}')
	out := []json.RawMessage{deep}

	m, isObject := decodeObject(valid)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	splice := func(first string, rest map[string]any) json.RawMessage {
		// {first, rest...}: first is a raw member the encoder would not
		// produce, rest the remaining members encoded as usual.
		b, _ := json.Marshal(rest)
		if len(rest) == 0 {
			return json.RawMessage("{" + first + "}")
		}
		return json.RawMessage("{" + first + "," + string(b[1:]))
	}
	switch {
	case isObject && len(keys) > 0:
		k := keys[0]
		vb, _ := json.Marshal(m[k])
		member := strconv.Quote(k) + ":" + string(vb)
		out = append(out, json.RawMessage("{"+member+","+member+string(mustMarshal(m)[1:])))
	default:
		out = append(out, json.RawMessage(`{"k":1,"k":1}`))
	}
	// A string property, when there is one, carries the bad bytes, so a
	// schema that forbids unknown properties is not what refuses them.
	target := "_"
	rest := map[string]any{}
	if isObject {
		rest = clone(m)
		for _, k := range keys {
			if ps, ok := s.Properties[k]; ok {
				ps = resolve(ps)
				if ps.Types != nil && len(ps.Types.ToStrings()) == 1 && ps.Types.ToStrings()[0] == "string" && ps.Enum == nil && ps.Const == nil {
					target = k
					break
				}
			}
		}
		delete(rest, target)
	}
	key := strconv.Quote(target)
	out = append(out,
		splice(key+":\"a\xffb\"", rest),
		splice(key+`:"\ud800"`, rest),
	)
	if len(valid) > 1 {
		out = append(out, valid[:len(valid)-1])
	}
	out = append(out, append(append(json.RawMessage(nil), valid...), []byte(" {}")...))
	return out
}

func decodeObject(raw json.RawMessage) (map[string]any, bool) {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(n int) string { return strconv.Itoa(n) }
