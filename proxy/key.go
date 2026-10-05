package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	agentrt "github.com/joeylking/agent-runtime"
)

// requestKey identifies a call across calls and processes: the SHA-256 of
// the registered tool name and agentrt.CanonicalJSON of its arguments, the
// form the runtime hashes approvals over. Object key order, whitespace,
// and how a string's characters are escaped do not matter; every number's
// literal and every string's contents do, so 1250.5 and 1250.50 are two
// requests, as they are two approvals. ok is false for arguments the
// runtime refuses where consumer JSON enters (malformed, too deep, invalid
// UTF-8, an unpaired surrogate escape, a repeated key, a number past its
// bound): they have no key, match no other call, and go to the gate,
// which records them as invalid. A registered name holds no NUL, so the
// separator cannot be forged.
func requestKey(tool string, args json.RawMessage) (key string, ok bool) {
	c, err := agentrt.CanonicalJSON(args)
	if err != nil {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(c)
	return hex.EncodeToString(h.Sum(nil)), true
}
