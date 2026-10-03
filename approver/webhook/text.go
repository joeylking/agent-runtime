package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joeylking/agent-runtime/approver"
	"github.com/joeylking/agent-runtime/trace"
	"github.com/joeylking/agent-runtime/view"
)

// Text renders an approval for a terminal or a chat message, in the shape
// cmd/agentrt prints: the kind, the tool, the policy's reason, the expiry,
// then each argument and presentation field on its own lines with its byte
// size, every value escaped with trace.Sanitize so model-chosen text cannot
// move the cursor, clear the screen, or forge the look of the prompt, and
// a closing line the renderer writes itself, so the last thing read is not
// text the model or the policy chose. A string over view.MaxFieldBytes is shown by its
// head and tail. It is what GET ...?format=text serves.
func Text(p approver.Pending) string {
	var b strings.Builder
	status := strings.ToUpper(string(p.Status))
	if p.Status == "" {
		status = "UNKNOWN"
	}
	fmt.Fprintf(&b, "APPROVAL %s  %s\n", trace.Sanitize(status), trace.Sanitize(p.ID))
	fmt.Fprintf(&b, "run     %s\n", trace.Sanitize(p.RunID))
	fmt.Fprintf(&b, "kind    %s\n", trace.Sanitize(p.Kind))
	fmt.Fprintf(&b, "tool    %s\n", trace.Sanitize(p.Request.Spec.Name))
	if p.Reason != "" {
		fmt.Fprintf(&b, "reason  %s\n", trace.Sanitize(p.Reason))
	}
	fmt.Fprintf(&b, "hash    %s\n", trace.Sanitize(p.Hash))
	if !p.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, "expires %s", p.ExpiresAt.UTC().Format(time.RFC3339))
		if p.Expired {
			b.WriteString(" (expired)")
		}
		b.WriteByte('\n')
	}
	if p.DecidedBy != "" {
		fmt.Fprintf(&b, "decided %s by %s\n", p.DecidedAt.UTC().Format(time.RFC3339), trace.Sanitize(p.DecidedBy))
	}
	if p.DecodeError != "" {
		fmt.Fprintf(&b, "unread  %s\n", trace.Sanitize(p.DecodeError))
	}
	b.WriteString("arguments (the tool call the model made; one field per line, byte count is the field's own size)\n")
	fields(&b, p.Request.Args)
	b.WriteString("presentation (supplied by the consumer's policy; it may contain model-chosen text)\n")
	fields(&b, p.Presentation)
	fmt.Fprintf(&b, "----- end of approval %s for tool %s -----\n", trace.Sanitize(p.ID), trace.Sanitize(p.Request.Spec.Name))
	return b.String()
}

// fields prints a JSON object one field per line, sorted by key; a value
// that is not an object is printed as one field named value.
func fields(b *strings.Builder, raw json.RawMessage) {
	if len(bytes.TrimSpace(raw)) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		field(b, "value", trace.Sanitize(view.BoundValue(string(raw)).(string)), len(raw))
		return
	}
	obj, ok := v.(map[string]any)
	if !ok {
		one(b, "value", v)
		return
	}
	if len(obj) == 0 {
		b.WriteString("  (no fields)\n")
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		one(b, k, obj[k])
	}
}

func one(b *strings.Builder, key string, v any) {
	orig, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(b, "  %s (unprintable)\n", trace.Sanitize(key))
		return
	}
	bounded, err := json.MarshalIndent(view.BoundValue(v), "", "  ")
	if err != nil {
		bounded = orig
	}
	field(b, key, string(bounded), len(orig))
}

func field(b *strings.Builder, key, value string, origBytes int) {
	fmt.Fprintf(b, "  %s (%d bytes)\n", trace.Sanitize(key), origBytes)
	for _, line := range strings.Split(value, "\n") {
		fmt.Fprintf(b, "    %s\n", trace.Sanitize(line))
	}
}
