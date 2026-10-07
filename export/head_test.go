package export_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/joeylking/agent-runtime/export"
)

// Head is the seq the follower delivered up to and that event's stored
// hash: the head of the chain as far as it went, which VerifyEvents up to
// that seq finds intact, and which stays the same while later events are
// committed and until they are delivered.
func TestFollower_HeadIsTheChainAsFarAsDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	writer := openStore(t, path)
	ctx := context.Background()
	f := &export.Follower{Store: openReadOnly(t, path), Cursor: &export.MemCursor{}, Once: true}
	if seq, hash, err := f.Head(ctx); seq != 0 || hash != "" || err != nil {
		t.Fatalf("nothing delivered: %d %q %v", seq, hash, err)
	}
	runScripted(t, writer, 2, `{"a":1}`)
	var c collect
	if err := f.Follow(ctx, c.sink); err != nil {
		t.Fatal(err)
	}
	seq, hash, err := f.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := c.all()[len(c.all())-1]
	head, want, _ := writer.ChainHead(ctx)
	if seq != last.Seq || seq != head || hash != want || len(hash) != 64 {
		t.Fatalf("head %d %s, delivered up to %d, chain head %d %s", seq, hash, last.Seq, head, want)
	}
	if rep, err := writer.VerifyEvents(ctx, 0, seq); err != nil || rep.Break != nil || rep.Hash != hash {
		t.Fatalf("verify up to the head: %+v %v", rep, err)
	}

	runScripted(t, writer, 1, `{"a":2}`)
	if again, h, _ := f.Head(ctx); again != seq || h != hash {
		t.Fatalf("head moved to %d before delivery", again)
	}
	if seq, hash, err := (&export.Follower{Store: writer}).Head(ctx); seq != 0 || hash != "" || err != nil {
		t.Fatalf("a follower with no cursor: %d %q %v", seq, hash, err)
	}
}
