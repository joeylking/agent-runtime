package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	agentrt "github.com/joeylking/agent-runtime"
)

// PolicyID is the identity of the policy this configuration gives the
// proxy, which the gate records with every decision and agentrt.Recheck
// compares: "agentrt-proxy/sha256:" and the hex SHA-256 of the canonical
// JSON of the parts of the configuration the policy decides from. Those
// are the policy map with agentrt.DefaultPolicy's outcomes under it, and
// for each server its name and, for each rule by the server's tool name,
// its side effect, outcome, fixed values, and rename, which name the tool,
// classify it, and enter the approval it asks for. Nothing else in the
// configuration changes a decision of the policy: the database, session,
// commands and endpoints, timeouts, descriptions, denied parameters, the
// hold, the repeat window, max_pending, and the approval and grant TTLs
// do not enter it. The proxy's rules across calls, the repeat window and
// the pending cap among them, are applied before the policy is asked and
// are not part of it.
func (c *Config) PolicyID() string {
	type rule struct {
		SideEffect agentrt.SideEffect         `json:"side_effect"`
		Outcome    agentrt.PolicyOutcome      `json:"outcome,omitempty"`
		Fixed      map[string]json.RawMessage `json:"fixed,omitempty"`
		Rename     string                     `json:"rename,omitempty"`
	}
	type server struct {
		Name  string          `json:"name"`
		Rules map[string]rule `json:"rules"`
	}
	in := struct {
		Policy  map[agentrt.SideEffect]agentrt.PolicyOutcome `json:"policy"`
		Servers []server                                     `json:"servers"`
	}{Policy: c.policy()}
	for _, s := range c.Servers {
		out := server{Name: s.Name, Rules: map[string]rule{}}
		for name, r := range s.Rules {
			out.Rules[name] = rule{SideEffect: r.SideEffect, Outcome: r.Outcome, Fixed: r.Fixed, Rename: r.Rename}
		}
		in.Servers = append(in.Servers, out)
	}
	sort.Slice(in.Servers, func(i, j int) bool { return in.Servers[i].Name < in.Servers[j].Name })
	raw, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	canon, err := agentrt.CanonicalJSON(raw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return "agentrt-proxy/sha256:" + hex.EncodeToString(sum[:])
}

// DefaultRecheckLimit is how many of a session's runs Recheck re-checks
// when RecheckOptions.Limit is zero.
const DefaultRecheckLimit = 50

// RecheckOptions say which runs Recheck re-checks, and against what.
type RecheckOptions struct {
	// RunID re-checks that one run. Empty re-checks the session's runs,
	// newest first.
	RunID string
	// Session is the session whose runs are re-checked; empty is the
	// configuration's own.
	Session string
	// Limit bounds how many of the session's runs are re-checked:
	// DefaultRecheckLimit when zero.
	Limit int
	// CurrentTools connects to the upstream servers, loads their pinned
	// tools as serving would, and hands each request its tool's spec as
	// loaded now instead of the one recorded. Log receives the load
	// reports.
	CurrentTools bool
	Log          io.Writer
}

// Recheck re-checks recorded runs under the policy cfg gives the proxy now,
// through agentrt.Recheck: each decision the proxy's policy made is handed
// again to the policy built from cfg, as it was handed then, and the
// report says which would now be decided otherwise. It opens the database
// read-only and writes nothing; it connects to the upstream servers only
// with CurrentTools. Without it the policy's tools are the configuration's
// rules, named and classified as serving would name and classify them.
//
// The proxy's policy decides from what it is handed and from the record,
// with one exception: whether a request re-runs an attempt whose outcome
// is unknown, which the proxy named in memory as it proposed the request.
// A re-check reads it from the interruption approval the policy then
// asked for; a re-run the policy denied left none, and a re-check under a
// policy that no longer denies it answers as for a fresh request. A run
// whose id the session's index names but that never began is passed over.
func Recheck(ctx context.Context, cfg *Config, opts RecheckOptions) ([]agentrt.RecheckReport, error) {
	if err := checkDatabase(cfg.Database); err != nil {
		return nil, err
	}
	store, err := agentrt.OpenExisting(cfg.Database, true)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	p := &Proxy{
		cfg:        cfg,
		session:    cfg.Session,
		decider:    classPolicy(cfg.policy()),
		tools:      map[string]*tool{},
		statusName: cfg.StatusToolName(),
		store:      store,
		policyID:   cfg.PolicyID(),
		recorded:   true,
	}
	p.reruns.m = map[string]*attempt{}
	defer func() {
		p.store = nil
		p.Close()
	}()
	var current []agentrt.Tool
	if opts.CurrentTools {
		if err := cfg.checkReach(); err != nil {
			return nil, err
		}
		current, err = p.load(ctx)
		if opts.Log != nil {
			for _, r := range p.reports {
				fmt.Fprint(opts.Log, r.String())
			}
		}
		if err != nil {
			return nil, err
		}
	} else {
		p.ruleTools()
	}
	ids := []string{opts.RunID}
	if opts.RunID == "" {
		session := opts.Session
		if session == "" {
			session = cfg.Session
		}
		limit := opts.Limit
		if limit == 0 {
			limit = DefaultRecheckLimit
		}
		if limit < 0 {
			return nil, fmt.Errorf("recheck: limit %d is negative", limit)
		}
		if ids, err = sessionRuns(ctx, store, session, limit); err != nil {
			return nil, err
		}
	}
	var out []agentrt.RecheckReport
	for _, id := range ids {
		rep, err := agentrt.Recheck(ctx, store, id, gatePolicy{p}, agentrt.RecheckOptions{Tools: current})
		if errors.Is(err, agentrt.ErrNotFound) && opts.RunID == "" {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("recheck run %s: %w", id, err)
		}
		out = append(out, rep)
	}
	return out, nil
}

// ruleTools registers, for a re-check without the upstream servers, a tool
// for every rule as serving names and classifies it: the policy needs
// nothing of a tool but its name, server, class, outcome, and fixed
// values, and the request carries the spec recorded.
func (p *Proxy) ruleTools() {
	for _, sc := range p.cfg.Servers {
		for _, remote := range sortedKeys(sc.Rules) {
			r := sc.Rules[remote]
			name := r.Rename
			if name == "" {
				name = sc.Name + "_" + remote
			}
			t := &tool{name: name, server: sc.Name, remote: remote, class: r.SideEffect, fixed: r.Fixed, outcome: r.Outcome,
				spec: agentrt.ToolSpec{Name: name, SideEffect: r.SideEffect}}
			p.tools[name] = t
			p.order = append(p.order, name)
		}
	}
}

// sessionRuns are the ids of the session's newest runs, at most limit, from
// the proxy's index, newest first.
func sessionRuns(ctx context.Context, store *agentrt.Store, session string, limit int) ([]string, error) {
	rows, err := store.DB().QueryContext(ctx, `SELECT run_id FROM proxy_calls WHERE session = ? ORDER BY id DESC LIMIT ?`, session, limit)
	if err != nil {
		return nil, fmt.Errorf("recheck: the database has no index of proxy calls: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
