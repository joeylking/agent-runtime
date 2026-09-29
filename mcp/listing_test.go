package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawServer answers JSON-RPC over the in-memory transports with bytes the
// test writes, so a test can send what no SDK server would: a tool listed
// twice, a listing that never ends, a number float64 cannot hold. tools is
// called with each tools/list cursor and returns the raw result.
func rawServer(t *testing.T, tools func(cursor string) string) Server {
	t.Helper()
	client, server := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := server.Connect(ctx)
	if err != nil {
		t.Fatalf("connect raw server: %v", err)
	}
	t.Cleanup(func() { cancel(); conn.Close() })
	go func() {
		for {
			msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			req, ok := msg.(*jsonrpc.Request)
			if !ok || !req.ID.IsValid() {
				continue
			}
			resp := &jsonrpc.Response{ID: req.ID}
			switch req.Method {
			case "initialize":
				resp.Result = json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"raw","version":"0"}}`)
			case "tools/list":
				var params struct {
					Cursor string `json:"cursor"`
				}
				_ = json.Unmarshal(req.Params, &params)
				resp.Result = json.RawMessage(tools(params.Cursor))
			default:
				resp.Error = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "not here"}
			}
			if err := conn.Write(ctx, resp); err != nil {
				return
			}
		}
	}()
	return Server{Name: "fs", DefaultTimeout: 2 * time.Second, transport: client}
}

// listing is a raw tools/list result offering one tool per schema, named
// t0, t1, and so on.
func listing(schemas ...string) string {
	parts := make([]string, len(schemas))
	for i, s := range schemas {
		parts[i] = fmt.Sprintf(`{"name":"t%d","description":"d","inputSchema":%s}`, i, s)
	}
	return `{"tools":[` + strings.Join(parts, ",") + `]}`
}

// legacyCanonical is canonical as it was before schemas were read from raw
// bytes: the SDK's decoded value, re-encoded. It is the oracle every hash
// pinned until then was computed with.
func legacyCanonical(v any) (json.RawMessage, error) {
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

// sdkSchemas lists a server's tools the way the SDK decodes them, which is
// what the legacy pin hashed.
func sdkSchemas(t *testing.T, s Server) map[string]any {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "oracle", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), s.transport, nil)
	if err != nil {
		t.Fatalf("oracle connect: %v", err)
	}
	defer session.Close()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("oracle list: %v", err)
	}
	out := map[string]any{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl.InputSchema
	}
	return out
}

// hashCompatibleSchemas hold only numbers whose float64 text denotes the
// same decimal as the literal, so the pin must write them byte for byte as
// the legacy pin did.
var hashCompatibleSchemas = []string{
	readSchema,
	writeSchema,
	rootSchema,
	`{"type":"object","properties":{"n":{"type":"integer","minimum":0,"maximum":100,"default":42,"multipleOf":1}}}`,
	`{"type":"object","properties":{"x":{"type":"number","minimum":0.1,"maximum":1e21,"default":-0,"exclusiveMinimum":-0.0}}}`,
	`{"type":"object","properties":{"x":{"type":"number","enum":[1.0,1e2,1E+2,100.00,12e0,2.50,0.000001,1e-7,1e20,1e-6,5e-324,1.7976931348623157e308,123456789012345680000,9007199254740992,-2.5e-3,3.14159,-1]}}}`,
	`{"type":"object","properties":{"héllo 世界":{"type":"string","description":"😀 \u00e9 \u2028 \u2029 <b>&amp;</b> \"quoted\" back\\slash \u0001\t\n","default":"<script>"}}}`,
	`{"type":"object","properties":{"a":{"type":"array","items":{"type":["string","null"]},"minItems":0,"maxItems":10,"examples":[[1,2.5,"x",null,true,false,{}]]}},"additionalProperties":false,"required":["a"]}`,
	`{  "type" : "object" , "required":[] ,"properties":{"z":{},"a":{"const":0.30000000000000004},"m":{"const":1.5e300}} }`,
	`{"type":"object","properties":{"s":{"description":"lone \ud800 surrogate, pair \ud83d\ude00, invalid ` + "\xff\xfe" + ` bytes, nul \u0000","enum":["\u003c\u003e\u0026"]}}}`,
	`{"type":"object","properties":{"d":{"const":1},"d":{"const":2}}}`,
}

func TestCanonicalSchema_MatchesTheLegacyPinByteForByte(t *testing.T) {
	s := rawServer(t, func(string) string { return listing(hashCompatibleSchemas...) })
	legacy := sdkSchemas(t, s)
	m, err := Pin(context.Background(), rawServer(t, func(string) string { return listing(hashCompatibleSchemas...) }))
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if len(m.Tools) != len(hashCompatibleSchemas) {
		t.Fatalf("pinned %d tools, want %d", len(m.Tools), len(hashCompatibleSchemas))
	}
	for i := range hashCompatibleSchemas {
		name := fmt.Sprintf("t%d", i)
		old, err := legacyCanonical(legacy[name])
		if err != nil {
			t.Fatalf("%s: legacy: %v", name, err)
		}
		pinned := m.Tools[name]
		if !bytes.Equal(pinned.InputSchema, old) {
			t.Errorf("%s:\n new %s\n old %s", name, pinned.InputSchema, old)
		}
		if pinned.SchemaHash != hashBytes(old) {
			t.Errorf("%s: hash %s, legacy %s", name, pinned.SchemaHash, hashBytes(old))
		}
		t.Logf("%s identical: %s", name, pinned.InputSchema)
	}
}

func TestCanonicalSchema_FakeServerSchemasHashAsBefore(t *testing.T) {
	f := newFake(t)
	for i, s := range hashCompatibleSchemas {
		f.add(fmt.Sprintf("t%d", i), "d", s, nil, echoHandler)
	}
	legacy := sdkSchemas(t, f.server())
	m := mustPin(t, f)
	for name, v := range legacy {
		old, err := legacyCanonical(v)
		if err != nil {
			t.Fatalf("%s: legacy: %v", name, err)
		}
		if got := m.Tools[name].InputSchema; !bytes.Equal(got, old) {
			t.Errorf("%s:\n new %s\n old %s", name, got, old)
		}
	}
}

func TestCanonicalSchema_KeepsWhatFloat64WouldLose(t *testing.T) {
	cases := map[string]string{
		`9007199254740993`:                     `9007199254740993`,
		`12345678901234567890`:                 `12345678901234567890`,
		`0.1000000000000000000001`:             `0.1000000000000000000001`,
		`0.10000000000000000000010`:            `0.1000000000000000000001`,
		`1.00000000000000000001e300`:           `1.00000000000000000001e+300`,
		`3.141592653589793238462643383279`:     `3.141592653589793238462643383279`,
		`-9007199254740993.000`:                `-9007199254740993`,
		`9.007199254740993E15`:                 `9007199254740993`,
		`0.00000012345678901234567890123`:      `1.2345678901234567890123e-7`,
		`123456789012345678901234567890`:       `1.2345678901234567890123456789e+29`,
		`100000000000000000000000000000000001`: `1.00000000000000000000000000000000001e+35`,
	}
	for lit, want := range cases {
		schema := `{"type":"object","properties":{"n":{"maximum":` + lit + `}}}`
		got, err := canonicalSchema(json.RawMessage(schema))
		if err != nil {
			t.Fatalf("%s: %v", lit, err)
		}
		if w := `{"properties":{"n":{"maximum":` + want + `}},"type":"object"}`; string(got) != w {
			t.Errorf("%s: got %s, want %s", lit, got, w)
		}
	}
}

// TestPin_SeesANumberChangeBeyondFloat64 is the audit's probe: the legacy
// pin hashed 9007199254740993 and 9007199254740992 alike.
func TestPin_SeesANumberChangeBeyondFloat64(t *testing.T) {
	schema := func(max string) string {
		return `{"type":"object","properties":{"n":{"type":"integer","maximum":` + max + `,"default":12345678901234567890}}}`
	}
	current := schema("9007199254740993")
	s := rawServer(t, func(string) string { return listing(current) })
	m, err := Pin(context.Background(), s)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got := string(m.Tools["t0"].InputSchema); !strings.Contains(got, "9007199254740993") || !strings.Contains(got, "12345678901234567890") {
		t.Fatalf("pinned %s, want the literals kept", got)
	}
	current = schema("9007199254740992")
	_, rep, err := Load(context.Background(), rawServer(t, func(string) string { return listing(current) }), m, Rules{"t0": {SideEffect: agentrt.ReadOnly}})
	if err == nil {
		rep.Connection.Close()
		t.Fatal("Load must refuse the only tool")
	}
	if got := refusal(rep, "t0"); !strings.Contains(got, "input schema changed") {
		t.Errorf("refusal = %q", got)
	}
}

// TestPin_SDKServerSchemaKeepsItsLiterals covers the same through the SDK's
// own server, which is what the fake is.
func TestPin_SDKServerSchemaKeepsItsLiterals(t *testing.T) {
	f := newFake(t)
	f.add("big", "d", `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993}}}`, nil, echoHandler)
	m := mustPin(t, f)
	f.replace("big", "d", `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740992}}}`, nil, echoHandler)
	_, rep, err := load(t, f, m, Rules{"big": {SideEffect: agentrt.ReadOnly}})
	if err == nil {
		t.Fatal("Load must refuse the only tool")
	}
	if got := refusal(rep, "big"); !strings.Contains(got, "input schema changed") {
		t.Errorf("refusal = %q", got)
	}
}

func TestPin_ToolListedTwiceFails(t *testing.T) {
	dup := `{"tools":[{"name":"a","description":"first","inputSchema":` + readSchema + `},{"name":"a","description":"second","inputSchema":` + writeSchema + `}]}`
	_, err := Pin(context.Background(), rawServer(t, func(string) string { return dup }))
	if err == nil || !strings.Contains(err.Error(), `"a" twice`) {
		t.Errorf("err = %v, want the duplicate named", err)
	}

	pages := func(cursor string) string {
		if cursor == "" {
			return `{"tools":[{"name":"a","inputSchema":` + readSchema + `}],"nextCursor":"2"}`
		}
		return `{"tools":[{"name":"a","inputSchema":` + writeSchema + `}]}`
	}
	_, err = Pin(context.Background(), rawServer(t, pages))
	if err == nil || !strings.Contains(err.Error(), `"a" twice`) {
		t.Errorf("err = %v, want a duplicate across pages named", err)
	}
}

func TestPin_ListingIsBounded(t *testing.T) {
	endless := func(cursor string) string {
		n := len(cursor)
		return fmt.Sprintf(`{"tools":[{"name":"t%d","inputSchema":%s}],"nextCursor":"%s"}`, n, readSchema, cursor+"x")
	}
	_, err := Pin(context.Background(), rawServer(t, endless))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d pages", MaxToolPages)) {
		t.Errorf("err = %v, want the page cap", err)
	}

	loop := func(string) string {
		return `{"tools":[],"nextCursor":"same"}`
	}
	_, err = Pin(context.Background(), rawServer(t, loop))
	if err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Errorf("err = %v, want a repeating cursor refused", err)
	}

	many := make([]string, MaxTools+1)
	for i := range many {
		many[i] = readSchema
	}
	_, err = Pin(context.Background(), rawServer(t, func(string) string { return listing(many...) }))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d tools", MaxTools)) {
		t.Errorf("err = %v, want the tool cap", err)
	}

	m, err := Pin(context.Background(), rawServer(t, func(string) string { return listing(many[:MaxTools]...) }))
	if err != nil || len(m.Tools) != MaxTools {
		t.Errorf("err = %v, want exactly MaxTools pinned", err)
	}
}
