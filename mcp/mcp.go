// Package mcp exposes the tools of an MCP server as agentrt tools, behind the
// runtime's policy instead of the server's own judgement.
//
// The MCP specification says a client must never make decisions from tool
// annotations, because an untrusted server supplies them. So every tool needs
// an operator [Rule] naming its side-effect class, and a tool without one is
// not registered. The tool set is pinned first: [Pin] records what each tool
// presents in a [Manifest] the operator reviews and stores, and [Load] refuses
// any tool whose live schema or description no longer hashes to the pin. A
// description is model input, so a changed description is an injection vector
// rather than a cosmetic edit.
//
// What the pin does not protect: it covers what a server presents, not what
// it does, so a server that behaves differently after Load, or differently
// from what its description says, is not caught. The tool set is fixed at
// Load and tools/list_changed notifications are ignored; a changed server is
// seen at the next Load. Annotations and output schemas are outside the
// hashes: annotations are recorded and can only refuse a registration, and
// an output schema is not checked at all. Load also refuses a pinned schema
// that could make the schema compiler look outside the document: a "$ref"
// that is not a fragment, or an "$id" or "$schema" that is not a well-known
// draft URI, anywhere in it.
//
// A stdio server is third-party code running as this process's user, with
// everything that user can reach. It does not inherit this process's
// environment: it gets PATH, HOME, and TMPDIR (the Windows equivalents on
// Windows), then every name Server.InheritEnv lists, taken from this
// process, then Server.Env as written. Only variables are withheld that way.
// The credential files under HOME, such as ~/.aws, ~/.ssh, or a token cache,
// and any directory on PATH the user can write, remain as reachable to the
// server as to the operator, and nothing about the host is sandboxed. A
// server that needs a credential is given that one variable through Env or
// InheritEnv and nothing else.
//
// A stdio server is started in a process group of its own, so a terminal's
// Ctrl-C reaches this process rather than the server, and Connection.Close
// kills the whole group once the SDK's own shutdown (close stdin, wait,
// SIGTERM, SIGKILL) has run, so a descendant, such as the node process a
// launcher script starts, does not outlive the session. On Linux the server is also killed if
// this process dies first; its own descendants are not, unless they watch
// for it. On other systems a server outlives a crash of this process until
// it reads the end of its stdin. On Windows no group is made, and Close ends
// only the server itself.
//
// The SDK reads the MCPGODEBUG environment variable once, when the program
// starts, to switch some of its behaviour back to older releases'; it is
// this process's variable, and a stdio server never receives it unless named.
// Nor does a stdio server receive HTTP_PROXY, HTTPS_PROXY, or NO_PROXY
// unless they are named, while this process's own HTTP clients, a
// streamable HTTP connection's included, honour all three through
// http.ProxyFromEnvironment unless Server.HTTPClient says otherwise.
//
// Connecting, the initialize handshake, and the first tool listing are
// bounded together by Server.ConnectTimeout (default 30s), so a server that
// never answers cannot hang Pin or Load; the session Load returns keeps
// working past that deadline once the phase completes. A listing itself is
// further bounded by MaxToolPages and MaxTools, so a server that pages
// forever or offers an unbounded tool set fails the listing rather than
// holding the client, and a streamable HTTP response over
// MaxResponseBodyBytes fails the same way. Load's own checks, the schema
// compile included, fall inside the same bound.
//
// A streamable HTTP connection follows no redirect unless Server.HTTPClient
// has a CheckRedirect of its own: a credential its RoundTripper adds would
// otherwise go wherever a 3xx points, and the session with it.
//
// Text a server chose, such as a tool name, a description, or an error
// message, is escaped by trace.Sanitize wherever this package puts it in
// text for an operator's terminal: Report.String and the errors of Pin and
// Load. The Report's fields and the Manifest keep the values as received.
//
// Resources, prompts, sampling, elicitation, and the server side of MCP are
// out of scope, and a call the server answers with an input request fails.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/joeylking/agent-runtime/trace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxContentBytes caps one result's recorded content when a Server
// names no limit of its own.
const DefaultMaxContentBytes = 64 << 10

// MaxToolPages and MaxTools bound one tool listing, so a server that pages
// forever or offers an unbounded tool set fails the listing instead of
// holding the client.
const (
	MaxToolPages = 100
	MaxTools     = 1000
)

// DefaultConnectTimeout bounds connecting, the initialize handshake, and the
// first tool listing when a Server names no ConnectTimeout of its own.
const DefaultConnectTimeout = 30 * time.Second

// clientName and clientVersion identify this adapter to a server.
const (
	clientName    = "agent-runtime"
	clientVersion = "0.1.0"
)

// Server describes one MCP server and what applies to every tool loaded from
// it.
type Server struct {
	// Name is required. It prefixes the names the model sees and names the
	// manifest, so tools from two servers cannot collide.
	Name string
	// Command is a stdio server's argv. Exactly one of Command or Endpoint
	// is set.
	Command []string
	// A stdio server does not inherit this process's environment. It gets
	// PATH, HOME, and TMPDIR (on Windows PATH, PATHEXT, SystemRoot,
	// SystemDrive, ComSpec, TEMP, TMP, USERPROFILE, APPDATA, LOCALAPPDATA,
	// and HOME) when they are set here, then every variable InheritEnv names
	// that is set here, then Env, entries of the form NAME=value. A later
	// entry replaces an earlier one of the same name. A server that needs a
	// token is handed that token and no other.
	Env        []string
	InheritEnv []string
	// Stderr receives a stdio server's stderr. When it is nil the last
	// StderrTailBytes are kept and added to a failed connect or tool
	// listing.
	Stderr io.Writer
	// Endpoint is a streamable HTTP server's URL, and HTTPClient is the
	// client used to reach it. The client is copied, never modified, and
	// its transport is wrapped so that no response body exceeds
	// MaxResponseBodyBytes; with no HTTPClient a fresh client is used, not
	// http.DefaultClient. No redirect is followed unless the client has a
	// CheckRedirect of its own, and then where it sends a credential is the
	// operator's responsibility. The standalone SSE stream a server could push
	// notifications on is not opened, since nothing here listens to them.
	Endpoint   string
	HTTPClient *http.Client
	// DefaultTimeout bounds a call for a Rule that names no timeout.
	DefaultTimeout time.Duration
	// ConnectTimeout bounds connecting, the initialize handshake, and the
	// first tool listing, together, as one phase; zero means
	// DefaultConnectTimeout. It does not shorten the life of the session Pin
	// or Load returns: once the phase completes, the connection keeps
	// working past this deadline under the caller's own context, because the
	// SDK does not tie a client session's lifetime to the context passed to
	// Connect for either a stdio or a streamable HTTP transport. On expiry
	// the error names which step timed out and, for a stdio server, includes
	// the stderr tail.
	ConnectTimeout time.Duration
	// MaxContentBytes caps one result's recorded content: the JSON the
	// runtime records, truncation marker included. Zero means
	// DefaultMaxContentBytes.
	MaxContentBytes int
	// Logger, when set, receives the MCP client's own logging.
	Logger *slog.Logger

	// transport replaces Command and Endpoint. Only the tests set it, with
	// the SDK's in-memory transports, so no test needs a subprocess or a
	// port.
	transport sdk.Transport
}

func (s Server) validate() error {
	if s.Name == "" {
		return errors.New("mcp: server name is required")
	}
	if err := validateEnv(s.Name, s.InheritEnv, s.Env); err != nil {
		return err
	}
	if s.transport != nil {
		return nil
	}
	switch {
	case len(s.Command) > 0 && s.Endpoint != "":
		return fmt.Errorf("mcp: server %q: Command and Endpoint are exclusive", s.Name)
	case len(s.Command) == 0 && s.Endpoint == "":
		return fmt.Errorf("mcp: server %q: one of Command or Endpoint is required", s.Name)
	}
	return nil
}

func (s Server) contentCap() int {
	if s.MaxContentBytes > 0 {
		return s.MaxContentBytes
	}
	return DefaultMaxContentBytes
}

func (s Server) connectTimeout() time.Duration {
	if s.ConnectTimeout > 0 {
		return s.ConnectTimeout
	}
	return DefaultConnectTimeout
}

// newTransport returns the transport, the stderr tail when one is kept, what
// to run once the SDK has started the server or failed to, and a stdio
// server's command, whose process group Close kills.
func (s Server) newTransport(rec *listRecorder) (sdk.Transport, *stderrTail, func(), *exec.Cmd, error) {
	switch {
	case s.transport != nil:
		return &recordingTransport{inner: s.transport, rec: rec}, nil, func() {}, nil, nil
	case len(s.Command) > 0:
		// Deliberately not CommandContext: the subprocess belongs to the
		// connection, not to the context that opened it, and Close stops it.
		cmd := exec.Command(s.Command[0], s.Command[1:]...)
		cmd.Env = childEnv(runtime.GOOS, s.InheritEnv, s.Env, os.LookupEnv)
		contain(cmd)
		var tail *stderrTail
		started := func() {}
		if s.Stderr != nil {
			cmd.Stderr = s.Stderr
		} else {
			t, w, err := newStderrTail()
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("mcp: server %q: stderr: %w", s.Name, err)
			}
			// The child holds its own copy of w once started; closing ours
			// is what lets the tail see the end of the stream.
			tail, cmd.Stderr, started = t, w, func() { w.Close() }
		}
		return &recordingTransport{inner: &sdk.CommandTransport{Command: cmd}, rec: rec}, tail, started, cmd, nil
	default:
		return &sdk.StreamableClientTransport{
			Endpoint:             s.Endpoint,
			HTTPClient:           httpClient(s.HTTPClient, rec),
			DisableStandaloneSSE: true,
		}, nil, func() {}, nil, nil
	}
}

// Connection is one client session. Every tool loaded from a server shares
// it and the consumer closes it when the run is over. There is no reconnect:
// once the session drops, every call fails.
type Connection struct {
	server         string
	session        *sdk.ClientSession
	rec            *listRecorder
	stderr         *stderrTail
	connectTimeout time.Duration
	cmd            *exec.Cmd
}

// Close ends the session, and with it a stdio server's subprocess: after the
// SDK's own shutdown sequence, whatever is left of the server's process
// group is killed, descendants included. Close is safe to call more than
// once and from more than one goroutine, including while a call is in
// flight, because sdk.ClientSession.Close is and killing a group that is
// gone does nothing.
func (c *Connection) Close() error {
	err := c.session.Close()
	killGroup(c.cmd)
	return err
}

// stderrWait bounds how long a failure waits for a dead server's stderr.
const stderrWait = 2 * time.Second

func connect(ctx context.Context, s Server) (*Connection, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, &sdk.ClientOptions{
		Logger: s.Logger,
		// The SDK would otherwise answer a server's input request itself.
		// Input-required results are refused instead, in Call, where they
		// become an observation the agent can read.
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	rec := &listRecorder{}
	transport, tail, started, cmd, err := s.newTransport(rec)
	if err != nil {
		return nil, err
	}
	session, err := client.Connect(ctx, transport, nil)
	started()
	if err != nil {
		killGroup(cmd)
		return nil, phaseErr(s.Name, "connect", s.connectTimeout(), tail, err)
	}
	return &Connection{server: s.Name, session: session, rec: rec, stderr: tail, connectTimeout: s.connectTimeout(), cmd: cmd}, nil
}

// phaseErr reports the failure of one bounded step of connecting: connect
// (which includes the initialize handshake, since the SDK's Connect does
// both) or list tools. When the step's own context deadline is what ended
// it, the error names the step and its bound rather than repeating
// "context deadline exceeded"; a stdio server's stderr tail is appended once
// its end of the pipe closes or stderrWait passes.
//
// What the server said, in an error message or on stderr, is escaped by
// trace.Sanitize, so it cannot rewrite the operator's terminal.
func phaseErr(server, step string, timeout time.Duration, tail *stderrTail, err error) error {
	msg := fmt.Errorf("mcp: server %q: %s: %w", server, step, sanitized{err})
	if errors.Is(err, context.DeadlineExceeded) {
		msg = fmt.Errorf("mcp: server %q: %s: timed out after %s", server, step, timeout)
	}
	wait, cancel := context.WithTimeout(context.Background(), stderrWait)
	defer cancel()
	return tail.annotate(wait, msg)
}

// sanitized is an error whose text is escaped by trace.Sanitize, and which
// still unwraps to the error it escapes.
type sanitized struct{ err error }

func (s sanitized) Error() string { return trace.Sanitize(s.err.Error()) }
func (s sanitized) Unwrap() error { return s.err }

// liveTool is one tool as the server listed it: the SDK's decoding for the
// description and annotations, and the canonical schema made from the bytes
// the server sent.
type liveTool struct {
	tool   *sdk.Tool
	schema json.RawMessage
}

// listTools lists every page of the server's tools. A failure closes the
// connection, so that a stdio server's stderr can be read in full.
func (c *Connection) listTools(ctx context.Context) (map[string]liveTool, error) {
	out, err := c.listPages(ctx)
	if err != nil {
		c.Close()
		return nil, phaseErr(c.server, "list tools", c.connectTimeout, c.stderr, err)
	}
	return out, nil
}

func (c *Connection) listPages(ctx context.Context) (map[string]liveTool, error) {
	out := map[string]liveTool{}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page == MaxToolPages {
			return nil, fmt.Errorf("more than %d pages", MaxToolPages)
		}
		c.rec.take()
		res, err := c.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		results := c.rec.take()
		if len(results) != 1 {
			return nil, errors.New("the raw tools/list result was not captured")
		}
		raw, err := rawTools(results[0])
		if err != nil {
			return nil, err
		}
		for name := range raw {
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("the server lists tool %q twice", name)
			}
		}
		if len(out)+len(raw) > MaxTools {
			return nil, fmt.Errorf("more than %d tools", MaxTools)
		}
		for _, t := range res.Tools {
			schema, ok := raw[t.Name]
			if !ok {
				return nil, fmt.Errorf("tool %q is missing from the raw listing", t.Name)
			}
			canon, err := canonicalSchema(schema)
			if err != nil {
				return nil, fmt.Errorf("tool %q: input schema: %w", t.Name, err)
			}
			out[t.Name] = liveTool{tool: t, schema: canon}
		}
		if res.NextCursor == "" {
			return out, nil
		}
		if seen[res.NextCursor] {
			return nil, fmt.Errorf("the cursor %q repeats", res.NextCursor)
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
}

// rawTools maps each tool in one raw tools/list result to its input schema
// as sent. Keys are matched exactly, as the SDK matches them, at every
// level: encoding/json would match a struct field case-insensitively, and
// then a page carrying both "tools" and "Tools" would pin one list while the
// SDK registered the other. A name listed twice on the page is an error.
func rawTools(result json.RawMessage) (map[string]json.RawMessage, error) {
	var page map[string]json.RawMessage
	if err := json.Unmarshal(result, &page); err != nil {
		return nil, fmt.Errorf("the raw tools/list result: %w", err)
	}
	var tools []map[string]json.RawMessage
	if raw, ok := page["tools"]; ok {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("the raw tools/list result: %w", err)
		}
	}
	out := make(map[string]json.RawMessage, len(tools))
	for _, t := range tools {
		var name string
		if err := json.Unmarshal(t["name"], &name); err != nil {
			continue // the SDK drops it too, and a tool it kept must be here
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("the server lists tool %q twice", name)
		}
		out[name] = t["inputSchema"]
	}
	return out, nil
}

// Manifest is what a server presented at the moment an operator reviewed it.
// Store it as JSON beside the Rules; Load compares the live server against it
// and refuses whatever changed.
type Manifest struct {
	Server   string                `json:"server"`
	PinnedAt time.Time             `json:"pinned_at"`
	Tools    map[string]PinnedTool `json:"tools"`
}

// PinnedTool is one tool as pinned. InputSchema is canonical JSON and the two
// hashes are what Load compares the live server against.
type PinnedTool struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	InputSchema     json.RawMessage `json:"input_schema"`
	SchemaHash      string          `json:"schema_hash"`
	DescriptionHash string          `json:"description_hash"`
	// Annotations are the server's own hints, recorded as received and nil
	// when it sent none. They are never trusted: they can only contradict an
	// operator's classification, never relax it. They are outside the
	// hashes, so a change to them alone does not refuse a tool.
	Annotations *sdk.ToolAnnotations `json:"annotations,omitempty"`
}

// Pin connects to the server, lists its tools, and records what each one
// presents. The operator reviews the manifest, stores it, and Load refuses
// anything that has changed since. A listing that names a tool twice fails.
func Pin(ctx context.Context, server Server) (*Manifest, error) {
	if err := server.validate(); err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, server.connectTimeout())
	defer cancel()
	conn, err := connect(pctx, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	live, err := conn.listTools(pctx)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Server: server.Name, PinnedAt: time.Now().UTC(), Tools: make(map[string]PinnedTool, len(live))}
	for name, t := range live {
		m.Tools[name] = PinnedTool{
			Name:            name,
			Description:     t.tool.Description,
			InputSchema:     t.schema,
			SchemaHash:      hashBytes(t.schema),
			DescriptionHash: hashString(t.tool.Description),
			Annotations:     t.tool.Annotations,
		}
	}
	return m, nil
}
