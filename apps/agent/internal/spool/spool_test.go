package spool_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/visionify/edgepilot/apps/agent/internal/spool"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func open(t *testing.T, dir string, opts spool.Options) *spool.Spool {
	t.Helper()
	opts.Dir = dir
	if opts.Logger == nil {
		opts.Logger = quiet()
	}
	s, err := spool.Open(opts)
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendN(t *testing.T, s *spool.Spool, n int, payload func(int) []byte) []uint64 {
	t.Helper()
	seqs := make([]uint64, 0, n)
	for i := range n {
		seq, err := s.Append(1, payload(i))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func body(i int) []byte { return fmt.Appendf(nil, "command-result-%04d", i) }

// --- the queue ------------------------------------------------------------

func TestRecordsComeBackInOrder(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{})
	appendN(t, s, 5, body)

	if got := s.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}

	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("peeked %d records, want 5", len(got))
	}
	for i, rec := range got {
		if want := body(i); !bytes.Equal(rec.Payload, want) {
			t.Errorf("record %d = %q, want %q", i, rec.Payload, want)
		}
		if rec.Seq != uint64(i+1) {
			t.Errorf("record %d has seq %d, want %d", i, rec.Seq, i+1)
		}
	}
}

// Peek must not consume: a record removed on send and never acknowledged is
// exactly the lost command result the spool exists to prevent.
func TestPeekDoesNotConsume(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{})
	appendN(t, s, 3, body)

	for range 3 {
		got, err := s.Peek(3)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("peek returned %d records, want 3 every time", len(got))
		}
	}
	if got := s.Len(); got != 3 {
		t.Errorf("Len = %d after three peeks, want 3", got)
	}
}

func TestCommitRemovesUpToAndIncludingTheSequence(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{})
	seqs := appendN(t, s, 5, body)

	if err := s.Commit(seqs[2]); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := s.Len(); got != 2 {
		t.Fatalf("Len = %d after committing 3 of 5, want 2", got)
	}

	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(got) != 2 || !bytes.Equal(got[0].Payload, body(3)) {
		t.Errorf("after commit the queue starts at %q, want %q", got[0].Payload, body(3))
	}
}

// The server acknowledges the highest sequence it processed, which after a
// replay may be one the agent has already committed.
func TestCommittingAnAlreadyCommittedSequenceIsHarmless(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{})
	seqs := appendN(t, s, 3, body)

	for range 3 {
		if err := s.Commit(seqs[1]); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if got := s.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
	if err := s.Commit(0); err != nil {
		t.Errorf("committing seq 0 should be a no-op, got: %v", err)
	}
	if got := s.Len(); got != 1 {
		t.Errorf("Len = %d after committing 0, want 1", got)
	}
}

func TestAnEmptySpoolPeeksToNothing(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{})
	got, err := s.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("peeked %d records from an empty spool", len(got))
	}
	if s.Len() != 0 || s.Bytes() != 0 {
		t.Errorf("empty spool reports %d records, %d bytes", s.Len(), s.Bytes())
	}
}

// --- surviving a restart ---------------------------------------------------

// The case this package exists for: the Mac reboots mid-outage and the command
// results taken before the reboot still reach the server.
func TestBufferedMessagesSurviveARestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := open(t, dir, spool.Options{})
	appendN(t, first, 4, body)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open(t, dir, spool.Options{})
	if got := second.Len(); got != 4 {
		t.Fatalf("Len = %d after reopening, want 4", got)
	}

	got, err := second.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	for i, rec := range got {
		if want := body(i); !bytes.Equal(rec.Payload, want) {
			t.Errorf("record %d = %q, want %q", i, rec.Payload, want)
		}
	}

	// The sequence must continue rather than restart, or a replay after the
	// reboot would collide with what came before it.
	next, err := second.Append(1, []byte("after-restart"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if next != 5 {
		t.Errorf("sequence after restart = %d, want 5 — it must not restart", next)
	}
}

// Committed records must stay committed. Otherwise every reboot during a long
// outage would re-send everything the server had already accepted.
func TestCommittedRecordsDoNotComeBackAfterARestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := open(t, dir, spool.Options{SegmentBytes: 256, MaxBytes: 1 << 20})
	seqs := appendN(t, first, 20, body)
	if err := first.Commit(seqs[14]); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open(t, dir, spool.Options{SegmentBytes: 256, MaxBytes: 1 << 20})
	got, err := second.Peek(100)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	// Whole segments are the unit of deletion, so a few already-committed
	// records may return; re-delivery is safe because the server is idempotent.
	// What must never come back is *everything*.
	if len(got) > 10 {
		t.Errorf("recovered %d records after committing 15 of 20; "+
			"committed records are not being freed", len(got))
	}
	for _, rec := range got {
		if rec.Seq <= 5 {
			t.Errorf("record %d came back after a whole segment of commits", rec.Seq)
		}
	}
}

// A power cut mid-write leaves a half-written record. Everything before it is
// still good and must survive; the torn tail must not be handed to the server
// as though it were a message.
func TestATornTailIsTruncatedAndTheRestSurvives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := open(t, dir, spool.Options{})
	appendN(t, first, 3, body)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Append a plausible-looking but incomplete record, as an interrupted write
	// would leave behind.
	segments, err := filepath.Glob(filepath.Join(dir, "spool-*.log"))
	if err != nil || len(segments) == 0 {
		t.Fatalf("no segments found: %v", err)
	}
	f, err := os.OpenFile(segments[0], os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if _, err := f.Write([]byte{0x45, 0x50, 0x52, 0x31, 0x00, 0x00}); err != nil {
		t.Fatalf("write torn record: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close segment: %v", err)
	}

	second := open(t, dir, spool.Options{})
	if got := second.Len(); got != 3 {
		t.Fatalf("Len = %d after a torn write, want the 3 intact records", got)
	}
	got, err := second.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("peeked %d records, want 3", len(got))
	}

	// And the spool must be usable afterwards, not stuck behind the damage.
	if _, err := second.Append(1, []byte("after-recovery")); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
}

// Bit rot, or a partially-written record whose length header happened to land.
// A record that fails its checksum must not be delivered as though it were
// intact.
func TestACorruptedRecordIsNotDelivered(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := open(t, dir, spool.Options{})
	appendN(t, first, 3, body)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	segments, _ := filepath.Glob(filepath.Join(dir, "spool-*.log"))
	data, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	// Flip a bit inside the second record's payload. The first record's header
	// is 22 bytes, its payload 19; the second record starts at 41.
	data[41+22+3] ^= 0xFF
	if err := os.WriteFile(segments[0], data, 0o600); err != nil {
		t.Fatalf("write segment: %v", err)
	}

	second := open(t, dir, spool.Options{})
	got, err := second.Peek(10)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	for _, rec := range got {
		if bytes.Contains(rec.Payload, []byte{0xFF}) {
			t.Errorf("a corrupted record was delivered: %q", rec.Payload)
		}
	}
	if len(got) < 1 {
		t.Error("the intact record before the corruption should still be delivered")
	}
	if len(got) > 1 {
		t.Errorf("delivered %d records; everything after the corruption is "+
			"untrustworthy and must be discarded", len(got))
	}
}

// --- the bound -------------------------------------------------------------

// The property the agent's memory and disk ceiling rests on: a six-hour outage
// must not produce a six-hour-sized spool.
func TestTheSpoolNeverGrowsPastItsCap(t *testing.T) {
	t.Parallel()

	const cap = 64 << 10
	s := open(t, t.TempDir(), spool.Options{MaxBytes: cap, SegmentBytes: 8 << 10})

	payload := bytes.Repeat([]byte("x"), 512)
	for range 2000 { // ~1 MiB written into a 64 KiB spool
		if _, err := s.Append(1, payload); err != nil {
			t.Fatalf("append: %v", err)
		}
		if got := s.Bytes(); got > cap {
			t.Fatalf("spool grew to %d bytes, above its %d cap", got, cap)
		}
	}

	if s.Dropped() == 0 {
		t.Error("records were evicted but Dropped reports none; loss must never be silent")
	}
	if s.Len() == 0 {
		t.Error("the spool evicted everything; the newest records must be kept")
	}
}

// Eviction takes the oldest first: a fresh command result is worth more than a
// six-hour-old one.
func TestEvictionDropsTheOldestFirst(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{MaxBytes: 8 << 10, SegmentBytes: 1 << 10})

	const total = 400
	appendN(t, s, total, func(i int) []byte { return fmt.Appendf(nil, "%08d", i) })

	got, err := s.Peek(1000)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("everything was evicted")
	}

	// Whatever survived must be a suffix of what was written.
	last := fmt.Appendf(nil, "%08d", total-1)
	if !bytes.Equal(got[len(got)-1].Payload, last) {
		t.Errorf("the newest record is %q, want %q — eviction is taking the wrong end",
			got[len(got)-1].Payload, last)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("records are out of order after eviction: %d then %d",
				got[i-1].Seq, got[i].Seq)
		}
	}
}

// Committed segments must free their space, or a healthy agent's spool would
// creep to the cap and start evicting for no reason.
func TestCommittedSpaceIsReclaimed(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{MaxBytes: 1 << 20, SegmentBytes: 1 << 10})

	payload := bytes.Repeat([]byte("y"), 256)
	var last uint64
	for range 100 {
		seq, err := s.Append(1, payload)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		last = seq
	}
	peak := s.Bytes()
	if peak == 0 {
		t.Fatal("the spool reports no bytes after 100 appends")
	}

	if err := s.Commit(last); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := s.Bytes(); got >= peak {
		t.Errorf("bytes = %d after committing everything, was %d; space is not reclaimed",
			got, peak)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("Len = %d after committing everything, want 0", got)
	}
	if s.Dropped() != 0 {
		t.Errorf("committing must not count as dropping: %d", s.Dropped())
	}
}

// A single oversized record must be refused rather than allowed to consume the
// whole spool.
func TestAnOversizedRecordIsRefused(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{})
	_, err := s.Append(1, make([]byte, spool.MaxRecordBytes+1))
	if !errors.Is(err, spool.ErrRecordTooLarge) {
		t.Errorf("expected ErrRecordTooLarge, got: %v", err)
	}
	if s.Len() != 0 {
		t.Error("a refused record must not be buffered")
	}
}

// --- concurrency -----------------------------------------------------------

// Collectors append from their own goroutines while the connection manager
// drains. The race detector is the point of this test.
func TestAppendAndDrainAreSafeConcurrently(t *testing.T) {
	t.Parallel()

	s := open(t, t.TempDir(), spool.Options{MaxBytes: 1 << 20, SegmentBytes: 16 << 10})

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				if _, err := s.Append(uint16(w), fmt.Appendf(nil, "w%d-%d", w, i)); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-done:
				return
			default:
			}
			batch, err := s.Peek(16)
			if err != nil {
				t.Errorf("peek: %v", err)
				return
			}
			if len(batch) == 0 {
				continue
			}
			if err := s.Commit(batch[len(batch)-1].Seq); err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			if s.Len() == 0 {
				return
			}
		}
	}()

	wg.Wait()
	<-done

	// Drain whatever the racing consumer left.
	for s.Len() > 0 {
		batch, err := s.Peek(64)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if err := s.Commit(batch[len(batch)-1].Seq); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if s.Dropped() != 0 {
		t.Errorf("nothing should have been evicted below the cap, dropped %d", s.Dropped())
	}
}

// --- lifecycle -------------------------------------------------------------

func TestUseAfterCloseIsRefused(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir(), Logger: quiet()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("closing twice should be a no-op, got: %v", err)
	}

	if _, err := s.Append(1, []byte("x")); !errors.Is(err, spool.ErrClosed) {
		t.Errorf("append after close: %v, want ErrClosed", err)
	}
	if _, err := s.Peek(1); !errors.Is(err, spool.ErrClosed) {
		t.Errorf("peek after close: %v, want ErrClosed", err)
	}
	if err := s.Commit(1); !errors.Is(err, spool.ErrClosed) {
		t.Errorf("commit after close: %v, want ErrClosed", err)
	}
}

func TestOpenRequiresADirectory(t *testing.T) {
	t.Parallel()

	if _, err := spool.Open(spool.Options{Logger: quiet()}); err == nil {
		t.Error("a spool without a directory must be refused")
	}
}

// An unrelated file in the spool directory is left alone rather than deleted:
// somebody may have put it there deliberately, and an agent that removes files
// it does not recognise is an agent nobody trusts with a directory.
func TestAnUnrecognisedFileIsLeftAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stranger := filepath.Join(dir, "README")
	if err := os.WriteFile(stranger, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := open(t, dir, spool.Options{})
	if _, err := s.Append(1, []byte("x")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("the spool removed a file it did not recognise: %v", err)
	}
}
