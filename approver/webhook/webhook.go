// Package webhook is the reference HTTP approval channel over
// approver.Approver: an http.Handler that shows one run's pending
// approvals, shows one approval as JSON or as sanitised plain text, and
// takes a decision bound to the hash it showed. It is what a chat bot or
// a browser page talks to; examples/approver is a thin client of it.
//
// # Routes
//
// Every route names the run, and nothing lists runs: a caller holding one
// run's id learns nothing about another's.
//
//	GET  /runs/{run}/approvals                 the run's pending approvals (JSON)
//	GET  /runs/{run}/approvals/{id}            one approval; JSON, or text with
//	                                           Accept: text/plain or ?format=text
//	POST /runs/{run}/approvals/{id}/approve    {"run_id","approval_id","hash","by","note"}
//	POST /runs/{run}/approvals/{id}/reject     the same body
//	POST /runs/{run}/cancel                    {"run_id","by","note"}
//
// A decision is accepted only when its body names the run and approval of
// the path, the hash the caller was shown, and an identity. approve
// answers 202: the run stays waiting until its owner resumes it. reject
// and cancel answer 200 with the run cancelled. Errors are JSON,
// {"error": text, "code": name}: 400 for a body that is not a decision,
// including one with no hash or no identity, which never reaches the
// Approver; 401 for a request that does not verify; 404 for a run or
// approval that does not exist under the run named, or any other route or
// method; 409 approval_changed when the stored approval is not the one
// shown, 409 not_pending when it is already decided, 409 run_not_waiting
// (run_finished for cancel) when the run is in another state, 409
// approval_hash when the stored row no longer matches its own hash; 410
// expired when the approval's expiry has passed; 413 for a body over
// Config.MaxBody; and 429 when Config.Rate is exceeded.
//
// # Verification
//
// Every request, GET included, carries three headers: HeaderTimestamp,
// the sender's clock as Unix seconds; HeaderNonce, a value the sender
// does not reuse, up to 64 characters; and HeaderSignature, "v1=" followed
// by the hex HMAC-SHA256 under Config.Secret of
//
//	timestamp "\n" nonce "\n" method "\n" path "\n" body
//
// where path is the request path as the handler sees it (after any
// http.StripPrefix) and body is the raw body, empty for GET. Sign and
// NewRequest produce it. The timestamp must be within Config.Skew of the
// handler's clock, the comparison is constant-time, and a signature seen
// once is refused again for as long as its timestamp could be accepted,
// so a captured request cannot be replayed inside the window; the nonce
// is what lets a client send the same decision twice in one second and
// have the second judged on its own. Requests are rate limited before
// they are read, and bodies are bounded.
//
// # Expiry
//
// A pending approval whose Limits.ApprovalTTL has passed is shown as 410
// without writing anything. A decision on it goes to the runtime, whose
// own expiry path marks it expired and cancels the run as
// approval_expired, and answers 410.
//
// # What this package does not do
//
// It has no users and no sessions: "by" is a label, recorded as given and
// never verified, exactly as cmd/agentrt's -by. The shared secret is the
// only authentication, and whoever holds it can decide any approval of
// any run whose id they know; hold it where the operator's chat or web
// front end runs, not in a browser. The secret never travels in a URL.
// Anyone who can write the database can approve anything without the
// secret, because the database, not this handler, is the trust boundary.
// It speaks plain HTTP: put it behind TLS and a network boundary the
// operator controls, and if a proxy adds a path prefix, strip it before
// the handler so the signed path is the one it sees. It does not resume
// runs, which needs the consumer's own process.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/approver"
)

// The headers every request carries.
const (
	HeaderTimestamp = "X-Approval-Timestamp"
	HeaderNonce     = "X-Approval-Nonce"
	HeaderSignature = "X-Approval-Signature"
)

// maxNonce bounds the nonce, which is part of what the handler remembers.
const maxNonce = 64

// Event types the handler delivers to Config.Observer. Neither is
// persisted (Seq is 0): the runtime's own approval.decided arrives through
// the Approver's observer when a decision commits.
const (
	// EventDecision records every approve, reject, or cancel request that
	// verified, with the status it was answered and the error if refused.
	EventDecision = "webhook.decision"
	// EventRefused records a request that did not verify, or exceeded the
	// rate, with the path and the reason.
	EventRefused = "webhook.refused"
)

// Config configures a Handler. Approver and Secret are required.
type Config struct {
	Approver approver.Approver
	// Secret signs every request; at least 32 bytes.
	Secret []byte
	// Skew is how far a request's timestamp may be from the handler's
	// clock, either way. Default 5 minutes.
	Skew time.Duration
	// Rate and Burst bound requests per second over the whole handler,
	// before anything is read. Defaults 10 and 20.
	Rate, Burst int
	// MaxBody bounds a request body in bytes. Default 16 KiB.
	MaxBody int64
	// Limit is how many pending approvals a listing returns. Default 20.
	Limit int
	// Observer, which may be nil, receives EventDecision and EventRefused.
	Observer agentrt.Observer
}

const minSecret = 32

// Handler is the http.Handler. Use New.
type Handler struct {
	cfg Config
	mux *http.ServeMux

	mu       sync.Mutex
	tokens   float64
	refilled time.Time
	seen     map[string]time.Time
	maxSeen  int
}

// New builds a Handler, refusing a Config without an Approver or with a
// secret under 32 bytes.
func New(cfg Config) (*Handler, error) {
	if cfg.Approver == nil {
		return nil, errors.New("webhook: Config.Approver is required")
	}
	if len(cfg.Secret) < minSecret {
		return nil, fmt.Errorf("webhook: Config.Secret must be at least %d bytes", minSecret)
	}
	if cfg.Skew <= 0 {
		cfg.Skew = 5 * time.Minute
	}
	if cfg.Rate <= 0 {
		cfg.Rate = 10
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 20
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 16 << 10
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 20
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), tokens: float64(cfg.Burst), refilled: time.Now(), seen: map[string]time.Time{}}
	// A signature is remembered until its timestamp plus the skew, which
	// is as long as that timestamp stays acceptable. Measured on the wall
	// clock that is up to twice the skew for a timestamp at the future edge
	// of the window, so the set is bounded by the rate over that span plus
	// the burst.
	h.maxSeen = cfg.Burst + cfg.Rate*int(2*cfg.Skew/time.Second) + 1
	h.mux.HandleFunc("GET /runs/{run}/approvals", h.list)
	h.mux.HandleFunc("GET /runs/{run}/approvals/{id}", h.show)
	h.mux.HandleFunc("POST /runs/{run}/approvals/{id}/approve", h.decide(true))
	h.mux.HandleFunc("POST /runs/{run}/approvals/{id}/reject", h.decide(false))
	h.mux.HandleFunc("POST /runs/{run}/cancel", h.cancel)
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "not_found", "no such route") })
	return h, nil
}

// Sign computes the HeaderSignature value for a request: "v1=" and the hex
// HMAC-SHA256 of timestamp, nonce, method, path, and body joined by
// newlines.
func Sign(secret []byte, timestamp time.Time, nonce, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%d\n%s\n%s\n%s\n", timestamp.Unix(), nonce, method, path)
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// NewRequest builds a signed request for a client of the handler: the
// method and URL as given, body as the body, and the three headers set
// from the wall clock and a random nonce. The signed path is the URL's
// path, so a client of a handler mounted behind a prefix the server
// strips must sign the stripped path and send the full one itself.
func NewRequest(ctx context.Context, secret []byte, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return nil, err
	}
	now, nonce := time.Now(), hex.EncodeToString(n[:])
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, Sign(secret, now, nonce, method, req.URL.Path, body))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// bodyKey carries the verified body to the route handlers.
type bodyKey struct{}

// ServeHTTP rate limits, bounds and reads the body, verifies the request,
// and only then routes it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.allow() {
		h.refused(r, 429, "rate limit exceeded")
		fail(w, 429, "rate_limited", "too many requests")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.refused(r, 413, "body too large")
			fail(w, 413, "body_too_large", fmt.Sprintf("body over %d bytes", h.cfg.MaxBody))
			return
		}
		fail(w, 400, "bad_request", "body could not be read")
		return
	}
	if reason := h.verify(r, body); reason != "" {
		h.refused(r, 401, reason)
		fail(w, 401, "unauthorized", reason)
		return
	}
	h.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), bodyKey{}, body)))
}

// allow takes a token from the bucket.
func (h *Handler) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	h.tokens += now.Sub(h.refilled).Seconds() * float64(h.cfg.Rate)
	h.refilled = now
	if h.tokens > float64(h.cfg.Burst) {
		h.tokens = float64(h.cfg.Burst)
	}
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}

// verify checks the timestamp, the signature, and that the signature has
// not been seen; it returns the reason a request is refused, or "".
func (h *Handler) verify(r *http.Request, body []byte) string {
	ts, err := strconv.ParseInt(r.Header.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return "missing or malformed " + HeaderTimestamp
	}
	now := time.Now()
	at := time.Unix(ts, 0)
	if d := now.Sub(at); d > h.cfg.Skew || -d > h.cfg.Skew {
		return "timestamp outside the accepted window"
	}
	nonce := r.Header.Get(HeaderNonce)
	if nonce == "" || len(nonce) > maxNonce {
		return "missing or oversized " + HeaderNonce
	}
	got := r.Header.Get(HeaderSignature)
	want := Sign(h.cfg.Secret, at, nonce, r.Method, r.URL.Path, body)
	if got == "" || !hmac.Equal([]byte(got), []byte(want)) {
		return "signature does not match"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sig, until := range h.seen {
		if now.After(until) {
			delete(h.seen, sig)
		}
	}
	if _, dup := h.seen[got]; dup {
		return "signature already used"
	}
	if len(h.seen) >= h.maxSeen {
		return "too many signatures in the window"
	}
	h.seen[got] = at.Add(h.cfg.Skew)
	return ""
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run")
	pending, err := h.cfg.Approver.Pending(r.Context(), runID, h.cfg.Limit)
	if err != nil {
		failErr(w, err, false)
		return
	}
	reply(w, 200, map[string]any{"run_id": runID, "pending": pending})
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	p, err := h.cfg.Approver.Show(r.Context(), r.PathValue("run"), r.PathValue("id"))
	if err != nil {
		failErr(w, err, false)
		return
	}
	if p.Expired || p.Status == agentrt.ApprovalExpired {
		fail(w, 410, "expired", fmt.Sprintf("approval %s expired at %s", p.ID, p.ExpiresAt.UTC().Format(time.RFC3339)))
		return
	}
	if r.URL.Query().Get("format") == "text" || strings.HasPrefix(r.Header.Get("Accept"), "text/plain") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		io.WriteString(w, Text(p))
		return
	}
	reply(w, 200, p)
}

// decision is a decision request's body.
type decision struct {
	RunID      string `json:"run_id"`
	ApprovalID string `json:"approval_id"`
	Hash       string `json:"hash"`
	By         string `json:"by"`
	Note       string `json:"note"`
}

func (h *Handler) decide(approve bool) http.HandlerFunc {
	action, done := "reject", "rejected"
	if approve {
		action, done = "approve", "approved"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		runID, id := r.PathValue("run"), r.PathValue("id")
		var d decision
		if err := json.Unmarshal(body(r), &d); err != nil {
			fail(w, 400, "bad_request", "body is not a JSON decision")
			return
		}
		if d.RunID != runID || d.ApprovalID != id {
			fail(w, 400, "bad_request", "body must name the run and approval of the path")
			return
		}
		if d.Hash == "" || d.By == "" {
			fail(w, 400, "bad_request", "hash and by are required")
			return
		}
		dec := approver.Decision{RunID: runID, ApprovalID: id, Hash: d.Hash, By: d.By, Note: d.Note}
		var err error
		if approve {
			err = h.cfg.Approver.Approve(r.Context(), dec)
		} else {
			err = h.cfg.Approver.Reject(r.Context(), dec)
		}
		status := 200
		if approve {
			status = 202
		}
		if err != nil {
			status = failErr(w, err, false)
		} else {
			reply(w, status, map[string]any{"status": done, "run_id": runID, "approval_id": id, "hash": d.Hash, "by": d.By})
		}
		h.decided(r, action, runID, id, d.By, status, err)
	}
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run")
	var d decision
	if err := json.Unmarshal(body(r), &d); err != nil {
		fail(w, 400, "bad_request", "body is not a JSON decision")
		return
	}
	if d.RunID != runID || d.By == "" {
		fail(w, 400, "bad_request", "body must name the run of the path and an identity")
		return
	}
	err := h.cfg.Approver.Cancel(r.Context(), runID, d.By, d.Note)
	status := 200
	if err != nil {
		status = failErr(w, err, true)
	} else {
		reply(w, 200, map[string]any{"status": "cancelled", "run_id": runID, "by": d.By})
	}
	h.decided(r, "cancel", runID, "", d.By, status, err)
}

func body(r *http.Request) []byte {
	b, _ := r.Context().Value(bodyKey{}).([]byte)
	return b
}

// failErr answers with the status the runtime's error maps to and returns
// it. cancel's ErrRunState means the run is already finished; a
// decision's means it is not waiting.
func failErr(w http.ResponseWriter, err error, cancel bool) int {
	status, code := 500, "internal"
	msg := err.Error()
	switch {
	case errors.Is(err, approver.ErrDecision):
		status, code = 400, "bad_request"
	case errors.Is(err, agentrt.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, approver.ErrExpired):
		status, code = 410, "expired"
	case errors.Is(err, agentrt.ErrApprovalChanged):
		status, code = 409, "approval_changed"
	case errors.Is(err, agentrt.ErrNotPending):
		status, code = 409, "not_pending"
	case errors.Is(err, agentrt.ErrRunState) && cancel:
		status, code = 409, "run_finished"
	case errors.Is(err, agentrt.ErrRunState):
		status, code = 409, "run_not_waiting"
	case errors.Is(err, agentrt.ErrApprovalHash):
		status, code = 409, "approval_hash"
	default:
		msg = "internal error"
	}
	fail(w, status, code, msg)
	return status
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	reply(w, status, map[string]string{"error": msg, "code": code})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) decided(r *http.Request, action, runID, approvalID, by string, status int, err error) {
	if h.cfg.Observer == nil {
		return
	}
	p := map[string]any{"action": action, "approval_id": approvalID, "by": by, "status": status, "remote": r.RemoteAddr}
	if err != nil {
		p["error"] = err.Error()
	}
	h.emit(runID, EventDecision, p)
}

func (h *Handler) refused(r *http.Request, status int, reason string) {
	if h.cfg.Observer == nil {
		return
	}
	h.emit("", EventRefused, map[string]any{"method": r.Method, "path": r.URL.Path, "status": status, "reason": reason, "remote": r.RemoteAddr})
}

func (h *Handler) emit(runID, typ string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.cfg.Observer(agentrt.Event{RunID: runID, At: time.Now(), Type: typ, Payload: b})
}
