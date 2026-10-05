package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/mcp"
)

// Defaults for what a Config leaves out.
const (
	DefaultHold         = 15 * time.Second
	DefaultRepeatWindow = 10 * time.Minute
	DefaultMaxPending   = 5
	DefaultApprovalTTL  = 24 * time.Hour
	DefaultGrantTTL     = 24 * time.Hour
	DefaultStatusTool   = "gate_status"
	// MinLeaseTTL is the shortest lease_ttl: the lease is renewed at a
	// third of it, and a renewal that waits on a busy database longer than
	// what is left loses a live call's lease.
	MinLeaseTTL = 3 * time.Second
)

// namePattern is the session name's character set: it is the first part of
// every run id, so it holds nothing a shell, a terminal, or a path would
// read as more than a name.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// toolNamePattern is the status tool's, which is the mcp package's for the
// names it registers, so the status tool is callable wherever they are.
var toolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Config is the proxy's one configuration file. It is JSON, read only when
// the current user owns it and nobody else can write it, and decoded with
// unknown fields and repeated keys refused, so a misspelled or doubled key
// is an error and never a default. Relative paths in it, the database and
// the manifests, are relative to the file's own directory.
type Config struct {
	// Database is the SQLite file the runs, approvals, and the proxy's
	// index of its calls are kept in. agentrt reads and decides on it.
	Database string `json:"database"`
	// Session names this proxy's calls: every run id is the session, a
	// dot, and a random suffix. Proxies sharing a session share its rules
	// on repeats, pending approvals, and interruptions.
	Session string `json:"session"`
	// Servers are the upstream MCP servers. At least one is required.
	Servers []ServerConfig `json:"servers"`
	// Policy maps each side-effect class to allow, require_approval, or
	// deny. A class it leaves out takes agentrt.DefaultPolicy's outcome,
	// and a class that has none there is denied.
	Policy map[agentrt.SideEffect]agentrt.PolicyOutcome `json:"policy,omitempty"`
	// Hold is how long a call that needs an approval is held open waiting
	// for it before it answers PENDING_APPROVAL. Zero answers at once.
	Hold *Duration `json:"hold,omitempty"`
	// RepeatWindow is how long after a call to a tool that is not
	// read-only executed an identical call is refused as a duplicate, and
	// a rejected or expired request is not asked again. Zero turns both
	// off.
	RepeatWindow *Duration `json:"repeat_window,omitempty"`
	// MaxPending caps the approvals this session may have waiting at once.
	MaxPending int `json:"max_pending,omitempty"`
	// ApprovalTTL and GrantTTL are agentrt.Limits': how long an approval
	// may stay pending, and how long a grant may wait to be collected.
	// Zero turns either off.
	ApprovalTTL *Duration `json:"approval_ttl,omitempty"`
	GrantTTL    *Duration `json:"grant_ttl,omitempty"`
	// StatusTool is the name the status tool is offered under.
	StatusTool string `json:"status_tool,omitempty"`
	// LeaseTTL is the lease each call holds on its run while it executes
	// (agentrt.GateConfig.LeaseTTL); zero is agentrt.DefaultLeaseTTL. A
	// call cut off by a crash is reported as in progress until it expires.
	LeaseTTL *Duration `json:"lease_ttl,omitempty"`

	// path is the file LoadConfig read, for the reach check (checkReach).
	path string
}

// ServerConfig is one upstream server: mcp.Server's fields, the manifest
// that pins it, and the operator's rules for its tools.
type ServerConfig struct {
	// Name prefixes the names its tools are offered under.
	Name string `json:"name"`
	// Command is a stdio server's argv; Endpoint a streamable HTTP
	// server's URL. Exactly one is set.
	Command []string `json:"command,omitempty"`
	// Env and InheritEnv are all of this process's environment a stdio
	// server is given beyond PATH, HOME, and TMPDIR (mcp.Server).
	Env        []string `json:"env,omitempty"`
	InheritEnv []string `json:"inherit_env,omitempty"`
	Endpoint   string   `json:"endpoint,omitempty"`
	// ConnectTimeout bounds connecting, the handshake, and the listing;
	// DefaultTimeout bounds a call whose rule names no timeout.
	ConnectTimeout *Duration `json:"connect_timeout,omitempty"`
	DefaultTimeout *Duration `json:"default_timeout,omitempty"`
	// Manifest is the file `agentrt-proxy pin` writes and serving reads.
	Manifest string `json:"manifest"`
	// Rules classify the server's tools by the server's own names. A tool
	// with no rule is not offered.
	Rules map[string]ToolRule `json:"rules"`
	// SkipPathCheck turns off, for this server, the refusal to start a
	// stdio server whose arguments name a directory holding the
	// configuration, a manifest, or the database (checkReach).
	SkipPathCheck bool `json:"skip_path_check,omitempty"`
}

// ToolRule is mcp.FileRule, the shape examples/mcp/rules.json uses, with
// two overrides for this one tool: the policy outcome, in place of its
// class's, and the repeat window.
type ToolRule struct {
	mcp.FileRule
	Outcome      agentrt.PolicyOutcome `json:"outcome,omitempty"`
	RepeatWindow *Duration             `json:"repeat_window,omitempty"`
}

// Duration is a time.Duration written as a Go duration string, "15s".
type Duration struct{ time.Duration }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"15s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration %q is negative", s)
	}
	d.Duration = v
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) or(def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return d.Duration
}

// LoadConfig reads the configuration at path through mcp.ReadOwned, so a
// file another user owns, or that its group or others can write, is
// refused: whoever can write it chooses the policy. Relative paths are
// resolved against the file's directory, and the configuration is checked
// before it is returned.
func LoadConfig(path string) (*Config, error) {
	raw, err := mcp.ReadOwned(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.resolve(filepath.Dir(abs))
	cfg.path = abs
	return cfg, nil
}

// ParseConfig decodes and checks a configuration, refusing unknown fields,
// a repeated key, which encoding/json would let the last one win, and
// anything after the one JSON object. Relative paths stay relative.
func ParseConfig(raw []byte) (*Config, error) {
	if err := usableJSON(raw); err != nil {
		return nil, err
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the configuration")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) resolve(dir string) {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	c.Database = abs(c.Database)
	for i := range c.Servers {
		c.Servers[i].Manifest = abs(c.Servers[i].Manifest)
	}
}

func validOutcome(o agentrt.PolicyOutcome) bool {
	switch o {
	case agentrt.Allow, agentrt.RequireApproval, agentrt.Deny:
		return true
	}
	return false
}

func validClass(s agentrt.SideEffect) bool {
	switch s {
	case agentrt.ReadOnly, agentrt.LocalMutation, agentrt.RemoteMutation, agentrt.Destructive:
		return true
	}
	return false
}

func (c *Config) validate() error {
	if c.Database == "" {
		return errors.New("database is required")
	}
	if !namePattern.MatchString(c.Session) {
		return fmt.Errorf("session %q must be 1 to 64 letters, digits, '_' or '-'", c.Session)
	}
	if len(c.Servers) == 0 {
		return errors.New("at least one server is required")
	}
	for class, outcome := range c.Policy {
		if !validClass(class) {
			return fmt.Errorf("policy: unknown side effect %q", class)
		}
		if !validOutcome(outcome) {
			return fmt.Errorf("policy: %s: outcome %q is not allow, require_approval, or deny", class, outcome)
		}
	}
	if c.MaxPending < 0 {
		return errors.New("max_pending must not be negative")
	}
	if c.LeaseTTL != nil && c.LeaseTTL.Duration < MinLeaseTTL {
		return fmt.Errorf("lease_ttl %s is shorter than %s", c.LeaseTTL.Duration, MinLeaseTTL)
	}
	if c.StatusTool != "" && !toolNamePattern.MatchString(c.StatusTool) {
		return fmt.Errorf("status_tool %q does not match %s", c.StatusTool, toolNamePattern)
	}
	seen := map[string]bool{}
	for i, s := range c.Servers {
		if s.Name == "" {
			return fmt.Errorf("servers[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("servers[%d]: the name %q is used twice", i, s.Name)
		}
		seen[s.Name] = true
		if (len(s.Command) == 0) == (s.Endpoint == "") {
			return fmt.Errorf("server %q: exactly one of command or endpoint is required", s.Name)
		}
		if s.Manifest == "" {
			return fmt.Errorf("server %q: manifest is required", s.Name)
		}
		for _, name := range sortedKeys(s.Rules) {
			r := s.Rules[name]
			if r.Outcome != "" && !validOutcome(r.Outcome) {
				return fmt.Errorf("server %q: tool %q: outcome %q is not allow, require_approval, or deny", s.Name, name, r.Outcome)
			}
			if _, err := r.Rule(); err != nil {
				return fmt.Errorf("server %q: tool %q: %w", s.Name, name, err)
			}
		}
	}
	return nil
}

// usableJSON refuses JSON the runtime would refuse from a consumer: above
// all a repeated key, which encoding/json decodes as the last one, so a
// file could say one thing to a reader and another to the proxy.
func usableJSON(raw []byte) error {
	if _, err := agentrt.CanonicalJSON(raw); err != nil {
		return fmt.Errorf("not usable JSON: %s", strings.TrimPrefix(err.Error(), "agentrt: canonical JSON: "))
	}
	return nil
}

// readManifest reads a manifest as mcp.ReadManifest does, through
// mcp.ReadOwned, refusing a repeated key as well.
func readManifest(path string) (*mcp.Manifest, error) {
	raw, err := mcp.ReadOwned(path)
	if err != nil {
		return nil, err
	}
	if err := usableJSON(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var m mcp.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// checkReach refuses, best effort, a stdio server whose arguments name the
// configuration, a manifest, or the database, or a directory holding one:
// the server runs as the same user, and one that can write them can
// rewrite the policy or forge an approval. An argument is taken as a path,
// relative to the proxy's working directory when it is not absolute, and
// so is the part after an '=' in it. Paths are compared absolute, cleaned,
// and with symbolic links resolved as far as they exist. It cannot see what a server reaches by other means, a
// path it is told later or one in its own configuration.
func (c *Config) checkReach() error {
	var protected []string
	for _, p := range append([]string{c.path, c.Database}, manifests(c)...) {
		if p == "" {
			continue
		}
		if r, err := realPath(p); err == nil {
			protected = append(protected, r)
		}
	}
	for _, s := range c.Servers {
		if s.SkipPathCheck || len(s.Command) < 2 {
			continue
		}
		for _, arg := range s.Command[1:] {
			candidates := []string{arg}
			if _, after, ok := strings.Cut(arg, "="); ok {
				candidates = append(candidates, after)
			}
			for _, cand := range candidates {
				if cand == "" {
					continue
				}
				reach, err := realPath(cand)
				if err != nil {
					continue
				}
				for _, p := range protected {
					if p == reach || strings.HasPrefix(p, reach+string(filepath.Separator)) || reach == string(filepath.Separator) {
						return fmt.Errorf("server %q: its argument %q reaches %s, which the proxy's policy and approvals depend on: "+
							"point the server elsewhere, or set skip_path_check on it if it cannot write there", s.Name, arg, p)
					}
				}
			}
		}
	}
	return nil
}

func manifests(c *Config) []string {
	var out []string
	for _, s := range c.Servers {
		out = append(out, s.Manifest)
	}
	return out
}

// realPath is p absolute, cleaned, and with symbolic links resolved, as
// far as it exists: a file not yet created, such as a new database, is
// resolved through its directory.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

// Server is the mcp.Server the entry describes. A stdio server's stderr is
// not passed through: the mcp package keeps its tail for a failed connect.
func (s ServerConfig) Server() mcp.Server {
	out := mcp.Server{
		Name:       s.Name,
		Command:    s.Command,
		Env:        s.Env,
		InheritEnv: s.InheritEnv,
		Endpoint:   s.Endpoint,
	}
	out.ConnectTimeout = s.ConnectTimeout.or(0)
	out.DefaultTimeout = s.DefaultTimeout.or(0)
	return out
}

// rules are the entry's mcp.Rules.
func (s ServerConfig) rules() (mcp.Rules, error) {
	out := make(mcp.Rules, len(s.Rules))
	for name, r := range s.Rules {
		rule, err := r.Rule()
		if err != nil {
			return nil, fmt.Errorf("server %q: tool %q: %w", s.Name, name, err)
		}
		out[name] = rule
	}
	return out, nil
}

// policy is the class map with agentrt.DefaultPolicy under it.
func (c *Config) policy() map[agentrt.SideEffect]agentrt.PolicyOutcome {
	out := map[agentrt.SideEffect]agentrt.PolicyOutcome{}
	for class, o := range agentrt.DefaultPolicy() {
		out[class] = o
	}
	for class, o := range c.Policy {
		out[class] = o
	}
	return out
}

// StatusToolName is the name the status tool is offered under.
func (c *Config) StatusToolName() string {
	if c.StatusTool == "" {
		return DefaultStatusTool
	}
	return c.StatusTool
}

func (c *Config) maxPending() int {
	if c.MaxPending == 0 {
		return DefaultMaxPending
	}
	return c.MaxPending
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
