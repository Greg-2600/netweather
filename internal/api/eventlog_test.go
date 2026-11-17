package api //nolint:testpackage // white-box tests cover the replay ring

import (
	"strings"
	"testing"
)

func TestEventLogAssignsOpaqueIDs(t *testing.T) {
	t.Parallel()

	log := newEventLog(4)
	first := log.append("state", []byte(`{"n":1}`))

	id := log.id(first.seq)
	if !strings.Contains(id, ":") {
		t.Fatalf("id = %q, want an epoch-prefixed id", id)
	}
	seq, ok := log.parseID(id)
	if !ok || seq != first.seq {
		t.Fatalf("parseID(%q) = %d, %v", id, seq, ok)
	}

	// An id from another process must not resume: the sequence number would
	// point at unrelated history.
	other := newEventLog(4)
	if _, ok := log.parseID(other.id(1)); ok {
		t.Fatal("an id from another epoch was accepted")
	}
	for _, bad := range []string{"", "nonsense", log.epoch + ":", log.epoch + ":x", log.epoch + ":-1"} {
		if _, ok := log.parseID(bad); ok {
			t.Errorf("parseID(%q) accepted a malformed id", bad)
		}
	}
	if _, ok := log.parseID(other.id(-5)); ok {
		t.Fatal("a negative sequence was accepted")
	}
}

func TestEventLogResumesWhatItCan(t *testing.T) {
	t.Parallel()

	log := newEventLog(4)
	first := log.append("state", []byte(`{"n":1}`))
	second := log.append("state", []byte(`{"n":2}`))
	third := log.append("incident", []byte(`{"n":3}`))

	// A client that is current gets nothing to replay.
	backlog, ch, ok, cancel := log.resume(log.id(third.seq))
	if !ok || len(backlog) != 0 {
		t.Fatalf("resume(current) = %+v, %v", backlog, ok)
	}
	cancel()
	if ch == nil {
		t.Fatal("a resumed client got no live channel")
	}

	// One behind gets exactly the missed event.
	backlog, _, ok, cancel = log.resume(log.id(second.seq))
	if !ok || len(backlog) != 1 || backlog[0].seq != third.seq {
		t.Fatalf("resume(second) = %+v, %v", backlog, ok)
	}
	cancel()

	// Two behind gets both.
	backlog, _, ok, cancel = log.resume(log.id(first.seq))
	if !ok || len(backlog) != 2 {
		t.Fatalf("resume(first) = %+v, %v", backlog, ok)
	}
	cancel()

	// A gap older than the ring is not bridged; the caller must snapshot.
	log.append("state", []byte(`{"n":4}`))
	log.append("state", []byte(`{"n":5}`))
	if _, _, ok, _ := log.resume(log.id(0)); ok {
		t.Fatal("a gap older than the ring was replayed")
	}

	// An unknown id is never replayable.
	if _, _, ok, _ := log.resume("garbage"); ok {
		t.Fatal("a garbage id was replayed")
	}
}

func TestEventLogEmptyIsCurrent(t *testing.T) {
	t.Parallel()

	log := newEventLog(2)
	backlog, _, ok, cancel := log.resume(log.id(0))
	if !ok || len(backlog) != 0 {
		t.Fatalf("empty log resume = %+v, %v", backlog, ok)
	}
	cancel()

	if got := log.current(); got != 0 {
		t.Fatalf("current = %d, want 0 before any event", got)
	}
}

func TestEventLogClosesASlowSubscriber(t *testing.T) {
	t.Parallel()

	log := newEventLog(256)
	backlog, ch, cancel := log.attach(log.current())
	defer cancel()
	if len(backlog) != 0 {
		t.Fatalf("backlog = %+v", backlog)
	}

	// The subscriber buffer is 64; the 65th event must close it rather than
	// silently drop a delta and leave the client diverged.
	for i := 0; i < 70; i++ {
		log.append("state", []byte(`{"n":1}`))
	}

	count := 0
	for range ch {
		count++
	}
	if count == 0 {
		t.Fatal("no event reached the subscriber before it was closed")
	}
	if count >= 70 {
		t.Fatalf("subscriber received %d events; a slow one was never closed", count)
	}
}
