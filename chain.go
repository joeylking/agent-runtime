package agentrt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
)

// The events are a hash chain. Each event's hash is the hex SHA-256 of
// seven netstrings, each the decimal length of a field in bytes, a colon,
// the field, and a comma: the previous event's hash, the empty string for
// the first event; the event's seq in decimal; then its run_id, step_id,
// at, type, and payload_json, each exactly as stored. The previous event
// is the one with the next lower seq across every run, so the chain is
// the order events were committed in. A netstring is unambiguous for any
// bytes, so no two different events encode alike, and it needs nothing
// but SHA-256 to recompute in another language.
//
// The chain proves integrity only against a head kept where whoever can
// write the database cannot reach: such a writer can recompute every hash
// after the row it changed. Within a database it catches corruption, a
// row altered, deleted, or reordered by something that did not also
// rewrite the chain from that row on, and an event missing from the end
// once the head is compared with one kept elsewhere.

// eventHash is an event's link in the chain, encoded into one buffer: it
// is computed for every event the runtime writes.
func eventHash(prev string, seq int64, runID, stepID, at, typ, payload string) string {
	var num [20]byte
	fields := [...]string{prev, string(strconv.AppendInt(num[:0], seq, 10)), runID, stepID, at, typ, payload}
	n := 0
	for _, f := range fields {
		n += len(f) + 22
	}
	b := make([]byte, 0, n)
	for _, f := range fields {
		b = strconv.AppendInt(b, int64(len(f)), 10)
		b = append(append(append(b, ':'), f...), ',')
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeField writes s to h as a netstring.
func writeField(h hash.Hash, s string) {
	fieldHeader(h, int64(len(s)))
	io.WriteString(h, s)
	h.Write([]byte{','})
}

func fieldHeader(h hash.Hash, n int64) {
	h.Write(strconv.AppendInt(nil, n, 10))
	h.Write([]byte{':'})
}

// VerifyReport is what VerifyEvents found.
type VerifyReport struct {
	// From and To are the seq of the first and last event checked, and
	// Checked how many there were; zero when the range holds none. A To
	// below the toSeq asked for means the database holds no event past
	// it: an intact chain cut at its end, which only a head kept
	// elsewhere at a later seq shows (agentrt verify -head).
	From, To int64
	Checked  int64
	// Hash is the stored hash of the last event checked: the head when To
	// is the newest event and Break is nil.
	Hash string
	// Break is the first event that does not chain, nil when every event
	// checked does. Checking stops there.
	Break *ChainBreak
}

// ChainBreak is an event whose stored hash is not the one its fields and
// the previous event's stored hash give, or whose seq does not follow the
// previous event's.
type ChainBreak struct {
	Seq int64
	// Expected is the hash recomputed from the previous event's stored
	// hash and this event's stored fields; Found is the hash stored, cut
	// at 128 characters.
	Expected string
	Found    string
	// Detail says what is wrong in words the runtime chose.
	Detail string
}

// chainPage is how many events VerifyEvents and the migration read per
// query, and chainInline how many bytes of each field the page carries:
// a longer field is read in chunks of MaxPageText bytes and hashed as it
// is read, so no event, however large, is held whole.
const (
	chainPage   = 128
	chainInline = 4096
	chainHash   = 128
)

// chainFields are the columns an event's hash covers after its seq, in
// order.
var chainFields = [...]string{"run_id", "step_id", "at", "type", "payload_json"}

// chainRow is an event as the chain reads it: each field's length in bytes
// and up to chainInline bytes of it.
type chainRow struct {
	seq    int64
	hash   string
	lens   [len(chainFields)]int64
	inline [len(chainFields)][]byte
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var chainSelect = func() string {
	q := `SELECT seq, substr(hash, 1, ` + strconv.Itoa(chainHash) + `)`
	for _, f := range chainFields {
		q += fmt.Sprintf(`, length(CAST(%[1]s AS BLOB)), substr(CAST(%[1]s AS BLOB), 1, %[2]d)`, f, chainInline)
	}
	return q + ` FROM events WHERE seq >= ? AND seq <= ? ORDER BY seq LIMIT ?`
}()

// chainRows reads at most chainPage events with seq from from to to.
func chainRows(ctx context.Context, q querier, from, to int64) ([]chainRow, error) {
	rows, err := q.QueryContext(ctx, chainSelect, from, to, chainPage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chainRow
	for rows.Next() {
		pageRow()
		var r chainRow
		dest := []any{&r.seq, &r.hash}
		for i := range chainFields {
			dest = append(dest, &r.lens[i], &r.inline[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// link recomputes r's hash after prev, reading what the page did not
// carry of a long field in chunks.
func (r *chainRow) link(ctx context.Context, q querier, prev string) (string, error) {
	h := sha256.New()
	writeField(h, prev)
	writeField(h, strconv.FormatInt(r.seq, 10))
	for i, f := range chainFields {
		fieldHeader(h, r.lens[i])
		h.Write(r.inline[i])
		for off := int64(len(r.inline[i])); off < r.lens[i]; {
			var chunk []byte
			if err := q.QueryRowContext(ctx, `SELECT substr(CAST(`+f+` AS BLOB), ?, ?) FROM events WHERE seq = ?`, off+1, MaxPageText, r.seq).Scan(&chunk); err != nil {
				return "", err
			}
			if len(chunk) == 0 {
				return "", fmt.Errorf("agentrt: event %d: %s changed while it was read", r.seq, f)
			}
			h.Write(chunk)
			off += int64(len(chunk))
		}
		h.Write([]byte{','})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// backfillChain computes the hash of every event a database held before
// the chain existed, in seq order from an empty previous hash, inside the
// migration's transaction. It reads nothing but the stored fields, so the
// same events always get the same hashes.
func backfillChain(ctx context.Context, tx *sql.Tx) error {
	prev, next := "", int64(1)
	for {
		page, err := chainRows(ctx, tx, next, 1<<63-1)
		if err != nil {
			return err
		}
		for i := range page {
			h, err := page[i].link(ctx, tx, prev)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE events SET hash = ? WHERE seq = ?`, h, page[i].seq); err != nil {
				return err
			}
			prev, next = h, page[i].seq+1
		}
		if len(page) < chainPage {
			return nil
		}
	}
}

// VerifyEvents walks the event chain from seq fromSeq to toSeq, both
// inclusive, and reports the first event that does not chain. fromSeq of
// zero or less starts at the first event, and toSeq of zero or less ends at
// the newest event when the walk starts. A range that does not start at
// the first event is checked against the stored hash of the event before
// it, which this call does not verify. An event whose seq does not follow
// the previous one's is a break: seq is never reused and a rolled-back
// write leaves no gap, so a gap is a deleted event.
//
// It reads a page of events at a time and each field in bounded chunks,
// so neither many events nor one huge event can exhaust memory, and it
// writes nothing. What it proves is limited: whoever can write the
// database can recompute every hash after a row they changed, so an
// intact chain means the record is unaltered only up to a head compared
// with one kept where that writer cannot reach (ChainHead, EventHash).
func (s *Store) VerifyEvents(ctx context.Context, fromSeq, toSeq int64) (VerifyReport, error) {
	if fromSeq < 1 {
		fromSeq = 1
	}
	if toSeq < 1 {
		head, _, err := s.ChainHead(ctx)
		if err != nil {
			return VerifyReport{}, err
		}
		toSeq = head
	}
	var rep VerifyReport
	if toSeq < fromSeq {
		return rep, nil
	}
	var prevSeq int64
	var prev string
	if fromSeq > 1 {
		err := s.db.QueryRowContext(ctx, `SELECT seq, substr(hash, 1, ?) FROM events WHERE seq < ? ORDER BY seq DESC LIMIT 1`, chainHash, fromSeq).Scan(&prevSeq, &prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return VerifyReport{}, err
		}
	}
	for next := fromSeq; next <= toSeq; {
		page, err := chainRows(ctx, s.db, next, toSeq)
		if err != nil {
			return VerifyReport{}, err
		}
		for i := range page {
			r := &page[i]
			want, err := r.link(ctx, s.db, prev)
			if err != nil {
				return VerifyReport{}, err
			}
			if rep.Checked == 0 {
				rep.From = r.seq
			}
			rep.To, rep.Checked, rep.Hash = r.seq, rep.Checked+1, r.hash
			switch {
			case r.seq != prevSeq+1 && prevSeq > 0:
				rep.Break = &ChainBreak{Seq: r.seq, Expected: want, Found: r.hash, Detail: fmt.Sprintf("seq %d follows %d: an event is missing", r.seq, prevSeq)}
			case r.seq != prevSeq+1 && fromSeq == 1:
				rep.Break = &ChainBreak{Seq: r.seq, Expected: want, Found: r.hash, Detail: fmt.Sprintf("the first event is seq %d: an event is missing", r.seq)}
			case r.hash != want:
				rep.Break = &ChainBreak{Seq: r.seq, Expected: want, Found: r.hash, Detail: "the stored hash is not the one the event's fields and the previous event's hash give"}
			}
			if rep.Break != nil {
				return rep, nil
			}
			prevSeq, prev = r.seq, r.hash
		}
		if len(page) < chainPage {
			break
		}
		next = page[len(page)-1].seq + 1
	}
	return rep, nil
}

// ChainHead returns the seq and stored hash of the newest event, the head
// of the chain, or zero and empty when there are none. Kept where whoever
// can write the database cannot reach, it is what a later VerifyEvents is
// compared with: an intact chain whose event at that seq still has that
// hash has not been rewritten up to it.
func (s *Store) ChainHead(ctx context.Context) (seq int64, hash string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT seq, substr(hash, 1, ?) FROM events ORDER BY seq DESC LIMIT 1`, chainHash).Scan(&seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return seq, hash, err
}

// EventHash returns the stored hash of the event at seq, cut at 128
// characters, which a follower records beside its cursor to anchor what it
// delivered. ErrNotFound when there is no such event.
func (s *Store) EventHash(ctx context.Context, seq int64) (string, error) {
	var h string
	err := s.db.QueryRowContext(ctx, `SELECT substr(hash, 1, ?) FROM events WHERE seq = ?`, chainHash, seq).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}
