// Package providers holds the dependency-free helpers every provider
// adapter shares: transport and HTTP error classification, a bounded body
// read, tool-use id synthesis, and price table helpers. It imports nothing
// outside the standard library and the runtime, so a provider SDK lives in
// its own module and the core stays untainted.
//
// The classification contract matches what the runtime's accounting caller
// expects. A timeout is returned bare, because the request may have been
// received and charged and the caller must record it as ambiguous; every
// other failure is either a TransientError, which the caller retries, or a
// permanent error, which ends the run.
package providers

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// MaxBodyBytes bounds how much of a provider response is read.
const MaxBodyBytes = 16 << 20

// maxExcerpt bounds how much of a body an error message repeats.
const maxExcerpt = 2048

// ReadBody reads at most MaxBodyBytes from r. A provider that answers with
// an unbounded stream cannot exhaust the process.
func ReadBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, MaxBodyBytes))
}

// Transient reports whether an HTTP status is worth retrying: 408 request
// timeout, 429 rate limited, and every 5xx, which includes Anthropic's 529
// overloaded.
func Transient(status int) bool {
	return status == 408 || status == 429 || status >= 500
}

// StatusError is a provider's non-2xx response. It carries the status so a
// caller can act on it without matching strings.
type StatusError struct {
	Provider string
	Status   int
	Body     string
}

func (e StatusError) Error() string {
	return fmt.Sprintf("%s: %d: %s", e.Provider, e.Status, e.Body)
}

// ClassifyTransport maps a failed HTTP round trip onto the runtime's
// classes. A timeout is returned bare so the accounting caller charges it
// as ambiguous; any other transport error is transient.
func ClassifyTransport(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return err
	}
	return agentrt.TransientError{Err: err}
}

// ClassifyStatus converts a provider response into an error, or nil for a
// 2xx. A retryable status yields a TransientError wrapping a StatusError;
// any other non-2xx yields the StatusError itself, which is permanent.
func ClassifyStatus(provider string, status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	err := StatusError{Provider: provider, Status: status, Body: excerpt(body)}
	if Transient(status) {
		return agentrt.TransientError{Err: err}
	}
	return err
}

// ClassifyAPIError classifies an SDK error that yields only a status code
// and the error itself. A status of zero means the SDK reported no HTTP
// response, so the error is a transport failure.
func ClassifyAPIError(status int, err error) error {
	if err == nil {
		return nil
	}
	if status == 0 {
		return ClassifyTransport(err)
	}
	if Transient(status) {
		return agentrt.TransientError{Err: err}
	}
	return err
}

func excerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxExcerpt {
		return s[:maxExcerpt] + "…"
	}
	return s
}

// SynthesizeToolUseID names the ith tool use of a response for providers
// that return no id. Tool-use ids only have to be unique within a request,
// and both consumers' adapters use this form.
func SynthesizeToolUseID(i int) string {
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
}
