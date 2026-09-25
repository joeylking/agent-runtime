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
// Resources, prompts, sampling, elicitation, and the server side of MCP are
// out of scope, and a call the server answers with an input request fails.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxContentBytes caps one result's content when a Server names no
// limit of its own.
const DefaultMaxContentBytes = 64 << 10

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
	// Command is a stdio server's argv and Env its environment; a nil Env
	// inherits this process's. Exactly one of Command or Endpoint is set.
	Command []string
	Env     []string
	// Endpoint is a streamable HTTP server's URL, and HTTPClient overrides
	// the client used to reach it.
	Endpoint   string
	HTTPClient *http.Client
	// DefaultTimeout bounds a call for a Rule that names no timeout.
	DefaultTimeout time.Duration
	// MaxContentBytes caps one result's content. Zero means
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

func (s Server) newTransport() sdk.Transport {
	switch {
	case s.transport != nil:
		return s.transport
	case len(s.Command) > 0:
		// Deliberately not CommandContext: the subprocess belongs to the
		// connection, not to the context that opened it, and Close stops it.
		cmd := exec.Command(s.Command[0], s.Command[1:]...)
		cmd.Env = s.Env
		return &sdk.CommandTransport{Command: cmd}
	default:
		return &sdk.StreamableClientTransport{Endpoint: s.Endpoint, HTTPClient: s.HTTPClient}
	}
}

// Connection is one client session. Every tool loaded from a server shares
// it and the consumer closes it when the run is over. There is no reconnect:
// once the session drops, every call fails.
type Connection struct {
	server  string
	session *sdk.ClientSession
}

// Close ends the session, and with it a stdio server's subprocess.
func (c *Connection) Close() error { return c.session.Close() }

func connect(ctx context.Context, s Server) (*Connection, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, &sdk.ClientOptions{
		Logger: s.Logger,
		// The SDK would otherwise answer a server's input request itself.
		// Input-required results are refused instead, in Call, where they
		// become an observation the agent can read.
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(ctx, s.newTransport(), nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: server %q: connect: %w", s.Name, err)
	}
	return &Connection{server: s.Name, session: session}, nil
}

func (c *Connection) listTools(ctx context.Context) (map[string]*sdk.Tool, error) {
	out := map[string]*sdk.Tool{}
	for t, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q: list tools: %w", c.server, err)
		}
		out[t.Name] = t
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
	// operator's classification, never relax it.
	Annotations *sdk.ToolAnnotations `json:"annotations,omitempty"`
}

// Pin connects to the server, lists its tools, and records what each one
// presents. The operator reviews the manifest, stores it, and Load refuses
// anything that has changed since.
func Pin(ctx context.Context, server Server) (*Manifest, error) {
	if err := server.validate(); err != nil {
		return nil, err
	}
	conn, err := connect(ctx, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	live, err := conn.listTools(ctx)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Server: server.Name, PinnedAt: time.Now().UTC(), Tools: make(map[string]PinnedTool, len(live))}
	for name, t := range live {
		schema, err := canonical(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q: tool %q: input schema: %w", server.Name, name, err)
		}
		m.Tools[name] = PinnedTool{
			Name:            name,
			Description:     t.Description,
			InputSchema:     schema,
			SchemaHash:      hashBytes(schema),
			DescriptionHash: hashString(t.Description),
			Annotations:     t.Annotations,
		}
	}
	return m, nil
}

// canonical re-encodes a value as JSON with sorted keys, no insignificant
// whitespace, and no HTML escaping, so that equal documents hash equally. The
// runtime hashes the same way; its own helper is not exported.
func canonical(v any) (json.RawMessage, error) {
	b, err := marshalCanonical(v)
	if err != nil {
		return nil, err
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return marshalCanonical(doc)
}

func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// hashString hashes a string through its JSON form, so descriptions and
// schemas are hashed over the same representation.
func hashString(s string) string {
	b, err := marshalCanonical(s)
	if err != nil {
		b = []byte(s)
	}
	return hashBytes(b)
}
