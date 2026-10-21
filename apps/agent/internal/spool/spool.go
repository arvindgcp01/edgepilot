// Package spool is the agent's bounded on-disk buffer for messages it could not
// deliver.
//
// # What it is for
//
// A Mac at a customer site loses its uplink for six hours. During that time it
// finishes commands, reports deployment progress, and records events. All of
// that has to survive until the server can be told, and none of it may grow
// without bound — the agent runs alongside the customer's actual workload, and
// an agent that fills the disk or the RAM is worse than an agent that lost a
// command result.
//
// # Durable versus perishable
//
// Only *durable* messages come here: command results, deployment progress,
// device events. Each describes something that happened once and cannot be
// recomputed.
//
// *Perishable* messages — metrics samples, heartbeats — never touch the spool.
// A five-minute-old CPU reading has no value, and buffering an hour of them
// would evict the command results that do. Dropping them is not a compromise;
// it is the correct handling.
//
// # The bound is the point
//
// The spool has a byte cap. When it is reached the oldest segment is deleted,
// which loses the oldest durable messages. That is a real loss, so it is
// counted, logged at warning, and reported to the server on reconnect — never
// silent. The alternative is an agent whose disk use is a function of how long
// the network stayed down, which is not a bound at all.
//
// # On-disk format
//
// Records are appended to segment files. Each record is:
//
//	magic  uint32   0x45505231 ("EPR1")
//	seq    uint64   spool sequence, monotonic across restarts
//	kind   uint16   caller-defined message kind, for metrics and diagnostics
//	length uint32   payload length
//	crc    uint32   CRC-32C of the payload
//	payload
//
// A record whose magic, length, or CRC does not check out ends the segment: the
// process died mid-write, and everything after that point in the file is
// untrustworthy. The file is truncated there and the records before it are kept.
package spool

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	recordMagic   uint32 = 0x45505231 // "EPR1"
	headerLen            = 4 + 8 + 2 + 4 + 4
	segmentPfx           = "spool-"
	segmentExt           = ".log"
	segmentDigits        = 20
)

// DefaultMaxBytes is the spool's ceiling, matching the figure in
// Architecture.md §3.2.
const DefaultMaxBytes int64 = 64 << 20

// DefaultSegmentBytes is how large a segment grows before the next one starts.
//
// Segments are the unit of deletion, both for acknowledged records and for
// enforcing the cap. Smaller segments free space sooner and waste less on a
// partially-acked segment; larger ones mean fewer files. 4 MiB gives sixteen
// segments at the default cap, so the cap is enforced with at most a sixteenth
// of it overshooting.
const DefaultSegmentBytes int64 = 4 << 20

// MaxRecordBytes bounds a single record.
//
// A record larger than this is a bug in the caller — the largest legitimate
// durable message is a command result with truncated output — and accepting it
// would let one message consume the whole spool.
const MaxRecordBytes = 8 << 20

var (
	// ErrRecordTooLarge reports a record above MaxRecordBytes.
	ErrRecordTooLarge = errors.New("spool: record exceeds the maximum record size")
	// ErrClosed reports use after Close.
	ErrClosed = errors.New("spool: the spool is closed")
)

// Record is one buffered message.
type Record struct {
	// Seq is assigned by the spool, monotonic across restarts. It is the
	// spool's own sequence, not the stream's: the stream's sequence restarts
	// with every connection, and this one must not.
	Seq uint64
	// Kind is caller-defined, carried through so metrics can distinguish a
	// backlog of command results from a backlog of events.
	Kind uint16
	// Payload is the marshalled message.
	Payload []byte
}

// Options configures a Spool.
type Options struct {
	// Dir is the spool directory. Created if absent.
	Dir string
	// MaxBytes caps the spool. Zero means DefaultMaxBytes.
	MaxBytes int64
	// SegmentBytes is the size at which a new segment starts. Zero means
	// DefaultSegmentBytes.
	SegmentBytes int64
	// Logger receives eviction warnings and recovery notes.
	Logger *slog.Logger
	// Sync forces each append to disk before returning.
	//
	// Default true. A command result that survives only in the page cache does
	// not survive the power cut that a customer site's cleaner causes, and
	// durable messages are low-volume by construction — this is not the hot
	// path.
	Sync *bool
}

// entry indexes one record without holding its payload.
//
// Payloads stay on disk: the whole point is that a six-hour backlog does not
// live in the agent's heap. At the default cap the index costs a few megabytes
// even for tiny records.
type entry struct {
	seq     uint64
	kind    uint16
	segment int // index into Spool.segments
	offset  int64
	length  int32
}

type segment struct {
	path  string
	first uint64
	bytes int64
	file  *os.File // non-nil only for the segment currently being written
}

// Spool is a bounded, crash-safe, on-disk queue.
//
// Safe for concurrent use: the collectors append from their own goroutines
// while the connection manager drains.
type Spool struct {
	dir          string
	maxBytes     int64
	segmentBytes int64
	log          *slog.Logger
	sync         bool

	mu       sync.Mutex
	closed   bool
	segments []*segment
	entries  []entry // pending records, oldest first
	head     int     // index into entries of the oldest pending record
	nextSeq  uint64
	bytes    int64

	// dropped counts records evicted by the cap. Reported, never silent.
	dropped uint64
}

// Open reads an existing spool or creates one.
//
// Recovery happens here: segments are scanned, a torn tail is truncated, and
// the sequence resumes after the highest record that survived.
func Open(opts Options) (*Spool, error) {
	if opts.Dir == "" {
		return nil, errors.New("spool: Dir is required")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = DefaultSegmentBytes
	}
	if opts.SegmentBytes > opts.MaxBytes {
		// A segment larger than the cap would mean the first eviction empties
		// the spool completely.
		opts.SegmentBytes = opts.MaxBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	shouldSync := true
	if opts.Sync != nil {
		shouldSync = *opts.Sync
	}

	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: create %s: %w", opts.Dir, err)
	}

	s := &Spool{
		dir:          opts.Dir,
		maxBytes:     opts.MaxBytes,
		segmentBytes: opts.SegmentBytes,
		log:          opts.Logger,
		sync:         shouldSync,
		nextSeq:      1,
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

// Append buffers a durable message and returns its spool sequence.
//
// Never blocks on the network and never grows past the cap. If the cap is
// reached the oldest segment is evicted first, so a fresh command result is
// preferred over a six-hour-old one.
func (s *Spool) Append(kind uint16, payload []byte) (uint64, error) {
	if len(payload) > MaxRecordBytes {
		return 0, fmt.Errorf("%w: %d bytes, maximum is %d",
			ErrRecordTooLarge, len(payload), MaxRecordBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}

	size := int64(headerLen + len(payload))

	// Evict before writing, so the cap is a ceiling rather than a line the
	// spool crosses and then retreats from.
	if err := s.evictFor(size); err != nil {
		return 0, err
	}

	seg, err := s.writableSegment(size)
	if err != nil {
		return 0, err
	}

	seq := s.nextSeq
	offset := seg.bytes
	if err := writeRecord(seg.file, seq, kind, payload, s.sync); err != nil {
		return 0, err
	}

	s.nextSeq++
	seg.bytes += size
	s.bytes += size
	s.entries = append(s.entries, entry{
		seq: seq, kind: kind,
		segment: len(s.segments) - 1, offset: offset, length: int32(len(payload)),
	})
	return seq, nil
}

// Peek returns up to n pending records, oldest first, without removing them.
//
// Removal is Commit's job, and only after the server has acknowledged: a record
// dropped on send and forgotten is exactly the lost command result the spool
// exists to prevent.
func (s *Spool) Peek(n int) ([]Record, error) {
	if n <= 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}

	pending := s.entries[s.head:]
	if len(pending) == 0 {
		return nil, nil
	}
	if n > len(pending) {
		n = len(pending)
	}

	out := make([]Record, 0, n)
	for _, e := range pending[:n] {
		payload, err := s.readAt(e)
		if err != nil {
			// A record that cannot be read is a record that will never be
			// deliverable. Returning what was read so far lets the caller make
			// progress rather than stalling the whole backlog behind one bad
			// entry.
			s.log.Error("reading a spooled record failed; it will be skipped",
				slog.Uint64("seq", e.seq), slog.Any("error", err))
			break
		}
		out = append(out, Record{Seq: e.seq, Kind: e.kind, Payload: payload})
	}
	return out, nil
}

// Commit removes every record up to and including seq.
//
// Called when the server acknowledges. Segments become removable only once
// every record in them is committed, which is why segments are sized well below
// the cap.
func (s *Spool) Commit(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}

	for s.head < len(s.entries) && s.entries[s.head].seq <= seq {
		s.head++
	}

	// Compact the index once most of it is consumed, so a long-lived agent's
	// index does not grow forever.
	if s.head > 0 && s.head*2 >= len(s.entries) {
		s.entries = append(s.entries[:0], s.entries[s.head:]...)
		s.head = 0
	}

	return s.dropUnreferencedSegments()
}

// Len returns the number of pending records.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) - s.head
}

// Bytes returns the spool's current on-disk size.
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Dropped returns how many records the cap has evicted.
//
// Surfaced as a metric and sent to the server, because silent loss is the one
// failure mode a buffer must never have.
func (s *Spool) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// NextSeq returns the sequence the next appended record will receive.
func (s *Spool) NextSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq
}

// Close releases the open segment. The spool's contents survive.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var errs []error
	for _, seg := range s.segments {
		if seg.file != nil {
			if err := seg.file.Close(); err != nil {
				errs = append(errs, fmt.Errorf("spool: close %s: %w", seg.path, err))
			}
			seg.file = nil
		}
	}
	return errors.Join(errs...)
}

// --- internals -------------------------------------------------------------

// writableSegment returns the segment to append size bytes to, rolling to a new
// one when the current is full.
func (s *Spool) writableSegment(size int64) (*segment, error) {
	if len(s.segments) > 0 {
		last := s.segments[len(s.segments)-1]
		if last.file != nil && last.bytes+size <= s.segmentBytes {
			return last, nil
		}
		if last.file != nil {
			if err := last.file.Close(); err != nil {
				return nil, fmt.Errorf("spool: close %s: %w", last.path, err)
			}
			last.file = nil
		}
	}

	path := filepath.Join(s.dir, segmentName(s.nextSeq))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spool: create segment %s: %w", path, err)
	}
	seg := &segment{path: path, first: s.nextSeq, file: file}
	s.segments = append(s.segments, seg)
	return seg, nil
}

// evictFor makes room for size bytes by deleting the oldest segments.
func (s *Spool) evictFor(size int64) error {
	for s.bytes+size > s.maxBytes && len(s.segments) > 0 {
		oldest := s.segments[0]

		// Never evict the segment currently being written: doing so would
		// discard the record about to be appended along with everything else
		// written since the last roll, and would leave an open file handle
		// pointing at a deleted path.
		if oldest.file != nil && len(s.segments) == 1 {
			break
		}

		evicted := 0
		for s.head < len(s.entries) && s.entries[s.head].segment == 0 {
			s.head++
			evicted++
		}
		s.dropped += uint64(evicted)

		if err := s.removeSegment(0); err != nil {
			return err
		}
		if evicted > 0 {
			s.log.Warn("the spool is full; the oldest buffered messages were discarded",
				slog.Int("records_dropped", evicted),
				slog.Uint64("dropped_total", s.dropped),
				slog.Int64("max_bytes", s.maxBytes))
		}
	}
	return nil
}

// dropUnreferencedSegments deletes segments no pending record points at.
func (s *Spool) dropUnreferencedSegments() error {
	for len(s.segments) > 0 {
		// The segment being written stays: more records are going into it.
		if s.segments[0].file != nil {
			break
		}
		if s.head < len(s.entries) && s.entries[s.head].segment == 0 {
			break
		}
		if err := s.removeSegment(0); err != nil {
			return err
		}
	}
	return nil
}

// removeSegment deletes segment i and reindexes the entries that referenced
// later segments.
func (s *Spool) removeSegment(i int) error {
	seg := s.segments[i]
	if seg.file != nil {
		if err := seg.file.Close(); err != nil {
			return fmt.Errorf("spool: close %s: %w", seg.path, err)
		}
		seg.file = nil
	}
	if err := os.Remove(seg.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("spool: remove %s: %w", seg.path, err)
	}

	s.bytes -= seg.bytes
	if s.bytes < 0 {
		s.bytes = 0
	}
	s.segments = append(s.segments[:i], s.segments[i+1:]...)
	for j := range s.entries {
		if s.entries[j].segment > i {
			s.entries[j].segment--
		}
	}
	return nil
}

// readAt reads one record's payload.
func (s *Spool) readAt(e entry) ([]byte, error) {
	seg := s.segments[e.segment]
	f, err := os.Open(seg.path)
	if err != nil {
		return nil, fmt.Errorf("spool: open %s: %w", seg.path, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, headerLen+int(e.length))
	if _, err := f.ReadAt(buf, e.offset); err != nil {
		return nil, fmt.Errorf("spool: read %s at %d: %w", seg.path, e.offset, err)
	}

	rec, _, err := parseRecord(buf)
	if err != nil {
		return nil, err
	}
	if rec.Seq != e.seq {
		return nil, fmt.Errorf("spool: %s at %d holds seq %d, index says %d",
			seg.path, e.offset, rec.Seq, e.seq)
	}
	return rec.Payload, nil
}

// recover rebuilds the index from the segment files.
func (s *Spool) recover() error {
	names, err := filepath.Glob(filepath.Join(s.dir, segmentPfx+"*"+segmentExt))
	if err != nil {
		return fmt.Errorf("spool: list segments in %s: %w", s.dir, err)
	}
	sort.Strings(names) // zero-padded, so lexical order is sequence order

	for _, path := range names {
		first, parseErr := parseSegmentName(filepath.Base(path))
		if parseErr != nil {
			// Not one of ours. Leaving it alone is safer than deleting a file
			// somebody put in the directory deliberately.
			s.log.Warn("ignoring an unrecognised file in the spool directory",
				slog.String("path", path))
			continue
		}

		seg := &segment{path: path, first: first}
		s.segments = append(s.segments, seg)
		segIdx := len(s.segments) - 1

		if err := s.recoverSegment(seg, segIdx); err != nil {
			return err
		}
	}

	// Drop leading segments that recovered empty — a segment created just
	// before a crash, or one whose records were all torn.
	for len(s.segments) > 0 && s.segments[0].bytes == 0 {
		if err := s.removeSegment(0); err != nil {
			return err
		}
	}

	if n := len(s.entries); n > 0 {
		s.nextSeq = s.entries[n-1].seq + 1
	}
	if n := len(s.entries); n > 0 {
		s.log.Info("recovered buffered messages from the spool",
			slog.Int("records", n), slog.Int64("bytes", s.bytes))
	}
	return nil
}

// recoverSegment scans one segment, indexing whole records and truncating at the
// first damaged one.
func (s *Spool) recoverSegment(seg *segment, segIdx int) error {
	data, err := os.ReadFile(seg.path)
	if err != nil {
		return fmt.Errorf("spool: read %s: %w", seg.path, err)
	}

	var offset int64
	for offset < int64(len(data)) {
		rec, n, parseErr := parseRecord(data[offset:])
		if parseErr != nil {
			// The process died mid-write. Everything from here on is
			// untrustworthy, so the file is truncated at the last good record.
			s.log.Warn("truncating a torn spool segment at the last intact record",
				slog.String("segment", filepath.Base(seg.path)),
				slog.Int64("offset", offset), slog.Any("reason", parseErr))
			if err := os.Truncate(seg.path, offset); err != nil {
				return fmt.Errorf("spool: truncate %s: %w", seg.path, err)
			}
			break
		}

		s.entries = append(s.entries, entry{
			seq: rec.Seq, kind: rec.Kind,
			segment: segIdx, offset: offset, length: int32(len(rec.Payload)),
		})
		offset += int64(n)
	}

	seg.bytes = offset
	s.bytes += offset
	return nil
}

func writeRecord(w io.Writer, seq uint64, kind uint16, payload []byte, sync bool) error {
	buf := make([]byte, headerLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:], recordMagic)
	binary.BigEndian.PutUint64(buf[4:], seq)
	binary.BigEndian.PutUint16(buf[12:], kind)
	binary.BigEndian.PutUint32(buf[14:], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[18:], crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
	copy(buf[headerLen:], payload)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("spool: write record: %w", err)
	}
	if sync {
		if f, ok := w.(*os.File); ok {
			if err := f.Sync(); err != nil {
				return fmt.Errorf("spool: sync: %w", err)
			}
		}
	}
	return nil
}

// parseRecord decodes one record and returns how many bytes it occupied.
func parseRecord(buf []byte) (Record, int, error) {
	if len(buf) < headerLen {
		return Record{}, 0, io.ErrUnexpectedEOF
	}
	if magic := binary.BigEndian.Uint32(buf[0:]); magic != recordMagic {
		return Record{}, 0, fmt.Errorf("bad record magic %#08x", magic)
	}

	seq := binary.BigEndian.Uint64(buf[4:])
	kind := binary.BigEndian.Uint16(buf[12:])
	length := binary.BigEndian.Uint32(buf[14:])
	crc := binary.BigEndian.Uint32(buf[18:])

	if length > MaxRecordBytes {
		return Record{}, 0, fmt.Errorf("record claims %d bytes, above the maximum", length)
	}
	total := headerLen + int(length)
	if len(buf) < total {
		return Record{}, 0, io.ErrUnexpectedEOF
	}

	payload := buf[headerLen:total]
	if got := crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)); got != crc {
		return Record{}, 0, fmt.Errorf("record %d fails its checksum", seq)
	}

	// Copied: the caller keeps this after the backing buffer is reused or the
	// mapped segment goes away.
	out := make([]byte, len(payload))
	copy(out, payload)
	return Record{Seq: seq, Kind: kind, Payload: out}, total, nil
}

func segmentName(first uint64) string {
	return fmt.Sprintf("%s%0*d%s", segmentPfx, segmentDigits, first, segmentExt)
}

func parseSegmentName(name string) (uint64, error) {
	if !strings.HasPrefix(name, segmentPfx) || !strings.HasSuffix(name, segmentExt) {
		return 0, fmt.Errorf("spool: %q is not a segment name", name)
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, segmentPfx), segmentExt)
	first, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("spool: %q has no sequence: %w", name, err)
	}
	return first, nil
}
