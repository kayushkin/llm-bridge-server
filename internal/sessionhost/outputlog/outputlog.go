// Package outputlog keeps what a harness wrote until the server has it.
//
// A session host gives every line a harness writes a sequence number, appends
// it here, and sends it. The server stores a line together with its number and
// then acknowledges; only then is the line dropped from the log. When the
// server comes back from a restart it names the last number it stored, and the
// host sends everything after it again. Nothing is ever dropped to make room:
// a log that has reached its allowance refuses the next entry, and the host
// stops reading the harness until the server acknowledges some.
//
// One log belongs to one spawn of one session, in a folder of its own. It is
// on disk so that a long gap costs disk, not memory, and so the log outlives
// the connection. It does not sync to disk on every entry: it is there to
// bridge a restart of the server, which the host process lives through, not a
// crash of the host's own machine.
package outputlog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kind says what an entry is.
type Kind byte

const (
	// KindOutputLine is one line the harness wrote to its standard output.
	KindOutputLine Kind = 1
	// KindExit is the harness process ending. It is the last entry of a log,
	// appended after the output has been read to its end, so it can never
	// arrive ahead of a line.
	KindExit Kind = 2
	// KindLineTooLong stands where a line longer than the host reads would
	// have been, so the server knows one was left out rather than see a gap
	// in the numbers.
	KindLineTooLong Kind = 3
)

func (kind Kind) valid() bool {
	return kind == KindOutputLine || kind == KindExit || kind == KindLineTooLong
}

// Entry is one numbered item of a log.
type Entry struct {
	// Sequence counts up from 1 within the log, with none skipped.
	Sequence uint64
	Kind     Kind
	Payload  []byte
}

// Options are the limits of one log.
type Options struct {
	// MaximumBytes is how much the log may hold on disk. Required.
	MaximumBytes int64
	// SegmentBytes is the size at which the log starts a new file. Whole
	// files are deleted as they are acknowledged, so this is also the most an
	// acknowledged entry can linger. Zero means 1 MiB.
	SegmentBytes int64
}

var (
	// ErrFull means the log has reached MaximumBytes. The entry was not
	// appended. The host must stop reading the harness and try again after
	// the server acknowledges.
	ErrFull = errors.New("outputlog: the log is full")
	// ErrEntryTooLarge means one entry is larger than the whole allowance, so
	// no acknowledgement would ever make room for it.
	ErrEntryTooLarge = errors.New("outputlog: the entry is larger than the log may hold")
	// ErrCorrupt means a file of the log does not read back as it was
	// written, somewhere other than a half-written last entry.
	ErrCorrupt = errors.New("outputlog: the log is corrupt")
	// ErrAlreadyDiscarded means entries were asked for that the log has
	// already dropped because they were acknowledged. The asker has lost
	// entries it once confirmed, which only a person can sort out.
	ErrAlreadyDiscarded = errors.New("outputlog: those entries were acknowledged and are gone")
	// ErrNeverAppended means an acknowledgement names a number the log has
	// not reached.
	ErrNeverAppended = errors.New("outputlog: that sequence was never appended")
	// ErrClosed means the log was used after Close.
	ErrClosed = errors.New("outputlog: the log is closed")
)

const (
	// An entry on disk: payload length, sequence, kind, the payload, then a
	// checksum of everything before it.
	entryHeaderBytes   = 4 + 8 + 1
	entryChecksumBytes = 4
	entryOverheadBytes = entryHeaderBytes + entryChecksumBytes

	defaultSegmentBytes  = 1 << 20
	segmentFileSuffix    = ".segment"
	acknowledgedFileName = "acknowledged"
)

// segment is one file of the log.
type segment struct {
	path          string
	firstSequence uint64
	// lastSequence is firstSequence-1 while the file holds no entry.
	lastSequence uint64
	bytes        int64
}

// Log is one spawn's output, numbered, on disk.
type Log struct {
	mutex    sync.Mutex
	folder   string
	options  Options
	segments []*segment
	// active is the last segment, open for appending; nil when there is none.
	active       *os.File
	lastSequence uint64
	acknowledged uint64
	bytes        int64
	closed       bool

	// Where the reader last stopped, so reading on from there does not scan
	// the file from its start each time.
	resumeAfter   uint64
	resumeSegment *segment
	resumeOffset  int64
}

// Open opens the log in folder, creating it if it does not exist. A log that
// was written before is read back: its numbering goes on where it stopped, and
// an entry that was only half written when the host died is cut off.
func Open(folder string, options Options) (*Log, error) {
	if options.MaximumBytes <= 0 {
		return nil, fmt.Errorf("outputlog: MaximumBytes must be set, got %d", options.MaximumBytes)
	}
	if options.SegmentBytes == 0 {
		options.SegmentBytes = defaultSegmentBytes
	}
	if options.SegmentBytes < 0 {
		return nil, fmt.Errorf("outputlog: SegmentBytes is %d", options.SegmentBytes)
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, fmt.Errorf("outputlog: %w", err)
	}
	log := &Log{folder: folder, options: options}

	acknowledged, err := readAcknowledged(folder)
	if err != nil {
		return nil, err
	}
	log.acknowledged = acknowledged
	log.lastSequence = acknowledged

	names, err := filepath.Glob(filepath.Join(folder, "*"+segmentFileSuffix))
	if err != nil {
		return nil, fmt.Errorf("outputlog: %w", err)
	}
	sort.Strings(names)
	for index, name := range names {
		firstSequence, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(name), segmentFileSuffix), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %s is not named by a sequence", ErrCorrupt, name)
		}
		isLast := index == len(names)-1
		read, err := readSegmentForOpen(name, firstSequence, isLast)
		if err != nil {
			return nil, err
		}
		if len(log.segments) > 0 {
			previous := log.segments[len(log.segments)-1]
			if read.firstSequence != previous.lastSequence+1 {
				return nil, fmt.Errorf("%w: %s starts at %d but the file before it ends at %d", ErrCorrupt, name, read.firstSequence, previous.lastSequence)
			}
		}
		log.segments = append(log.segments, read)
		log.bytes += read.bytes
		if read.lastSequence > log.lastSequence {
			log.lastSequence = read.lastSequence
		}
	}
	if len(log.segments) > 0 {
		first := log.segments[0]
		if first.firstSequence > log.acknowledged+1 {
			return nil, fmt.Errorf("%w: entries %d to %d are neither acknowledged nor on disk", ErrCorrupt, log.acknowledged+1, first.firstSequence-1)
		}
		last := log.segments[len(log.segments)-1]
		file, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("outputlog: %w", err)
		}
		log.active = file
	}
	// Entries acknowledged before the host stopped may still be in files it
	// had not yet deleted.
	if err := log.deleteAcknowledgedSegments(); err != nil {
		log.Close()
		return nil, err
	}
	return log, nil
}

func readAcknowledged(folder string) (uint64, error) {
	text, err := os.ReadFile(filepath.Join(folder, acknowledgedFileName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("outputlog: %w", err)
	}
	acknowledged, err := strconv.ParseUint(strings.TrimSpace(string(text)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: the acknowledged mark reads %q", ErrCorrupt, text)
	}
	return acknowledged, nil
}

// readSegmentForOpen reads one file from start to end to learn what it holds.
// A broken entry at the end of the last file is the entry being written when
// the host died, and is cut off. A broken entry anywhere else is corruption.
func readSegmentForOpen(path string, firstSequence uint64, isLast bool) (*segment, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("outputlog: %w", err)
	}
	defer file.Close()
	read := &segment{path: path, firstSequence: firstSequence, lastSequence: firstSequence - 1}
	for {
		entry, size, err := readEntry(file)
		if err == io.EOF {
			return read, nil
		}
		if err != nil {
			if !isLast {
				return nil, fmt.Errorf("%w: %s, after entry %d: %v", ErrCorrupt, path, read.lastSequence, err)
			}
			if err := file.Truncate(read.bytes); err != nil {
				return nil, fmt.Errorf("outputlog: cutting off a half-written entry in %s: %w", path, err)
			}
			return read, nil
		}
		if entry.Sequence != read.lastSequence+1 {
			return nil, fmt.Errorf("%w: %s holds entry %d where %d belongs", ErrCorrupt, path, entry.Sequence, read.lastSequence+1)
		}
		read.lastSequence = entry.Sequence
		read.bytes += size
	}
}

var errBrokenEntry = errors.New("an entry is incomplete or fails its checksum")

// readEntry reads the entry at the reader's position. It returns io.EOF when
// the reader is exactly at its end, and errBrokenEntry for anything that is
// not a whole, intact entry.
func readEntry(reader io.Reader) (Entry, int64, error) {
	var header [entryHeaderBytes]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		if err == io.EOF {
			return Entry{}, 0, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return Entry{}, 0, errBrokenEntry
		}
		return Entry{}, 0, err
	}
	payloadBytes := binary.BigEndian.Uint32(header[0:4])
	sequence := binary.BigEndian.Uint64(header[4:12])
	kind := Kind(header[12])
	rest := make([]byte, int(payloadBytes)+entryChecksumBytes)
	if _, err := io.ReadFull(reader, rest); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Entry{}, 0, errBrokenEntry
		}
		return Entry{}, 0, err
	}
	payload := rest[:payloadBytes]
	checksum := crc32.NewIEEE()
	checksum.Write(header[:])
	checksum.Write(payload)
	if checksum.Sum32() != binary.BigEndian.Uint32(rest[payloadBytes:]) || !kind.valid() {
		return Entry{}, 0, errBrokenEntry
	}
	return Entry{Sequence: sequence, Kind: kind, Payload: payload}, int64(entryOverheadBytes) + int64(payloadBytes), nil
}

func encodeEntry(sequence uint64, kind Kind, payload []byte) []byte {
	encoded := make([]byte, entryHeaderBytes+len(payload)+entryChecksumBytes)
	binary.BigEndian.PutUint32(encoded[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint64(encoded[4:12], sequence)
	encoded[12] = byte(kind)
	copy(encoded[entryHeaderBytes:], payload)
	binary.BigEndian.PutUint32(encoded[entryHeaderBytes+len(payload):], crc32.ChecksumIEEE(encoded[:entryHeaderBytes+len(payload)]))
	return encoded
}

// Append adds one entry and returns the sequence number it was given. It
// returns ErrFull, having appended nothing, when the log is at its allowance.
func (log *Log) Append(kind Kind, payload []byte) (uint64, error) {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	if log.closed {
		return 0, ErrClosed
	}
	if !kind.valid() {
		return 0, fmt.Errorf("outputlog: %d is not a kind of entry", kind)
	}
	entryBytes := int64(entryOverheadBytes) + int64(len(payload))
	if entryBytes > log.options.MaximumBytes || int64(len(payload)) > int64(^uint32(0)) {
		return 0, fmt.Errorf("%w: %d bytes against an allowance of %d", ErrEntryTooLarge, entryBytes, log.options.MaximumBytes)
	}
	if log.bytes+entryBytes > log.options.MaximumBytes {
		return 0, ErrFull
	}

	sequence := log.lastSequence + 1
	if log.active == nil || log.segments[len(log.segments)-1].bytes >= log.options.SegmentBytes {
		if err := log.startSegment(sequence); err != nil {
			return 0, err
		}
	}
	current := log.segments[len(log.segments)-1]
	if _, err := log.active.Write(encodeEntry(sequence, kind, payload)); err != nil {
		// Part of the entry may be on disk. Cut it off so the file still ends
		// on a whole entry and the same number can be given out again.
		if truncateErr := log.active.Truncate(current.bytes); truncateErr != nil {
			return 0, fmt.Errorf("outputlog: appending entry %d: %w; and the partial entry could not be removed: %v", sequence, err, truncateErr)
		}
		return 0, fmt.Errorf("outputlog: appending entry %d: %w", sequence, err)
	}
	current.lastSequence = sequence
	current.bytes += entryBytes
	log.bytes += entryBytes
	log.lastSequence = sequence
	return sequence, nil
}

func (log *Log) startSegment(firstSequence uint64) error {
	if log.active != nil {
		if err := log.active.Close(); err != nil {
			return fmt.Errorf("outputlog: %w", err)
		}
		log.active = nil
	}
	path := filepath.Join(log.folder, fmt.Sprintf("%020d%s", firstSequence, segmentFileSuffix))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("outputlog: %w", err)
	}
	log.active = file
	log.segments = append(log.segments, &segment{path: path, firstSequence: firstSequence, lastSequence: firstSequence - 1})
	return nil
}

// Acknowledge records that the server has stored every entry up to and
// including through, and drops the files that hold nothing later. An
// acknowledgement at or below the last one changes nothing.
func (log *Log) Acknowledge(through uint64) error {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	if log.closed {
		return ErrClosed
	}
	if through > log.lastSequence {
		return fmt.Errorf("%w: %d, and the log ends at %d", ErrNeverAppended, through, log.lastSequence)
	}
	if through <= log.acknowledged {
		return nil
	}
	// The mark goes to disk before any file is deleted. Were the host to die
	// between the two, it would find acknowledged entries still on disk, which
	// is harmless; the other order could leave a gap nothing explains.
	temporary := filepath.Join(log.folder, acknowledgedFileName+".writing")
	if err := os.WriteFile(temporary, []byte(strconv.FormatUint(through, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("outputlog: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(log.folder, acknowledgedFileName)); err != nil {
		return fmt.Errorf("outputlog: %w", err)
	}
	log.acknowledged = through
	return log.deleteAcknowledgedSegments()
}

func (log *Log) deleteAcknowledgedSegments() error {
	for len(log.segments) > 0 {
		oldest := log.segments[0]
		if oldest.lastSequence > log.acknowledged {
			return nil
		}
		if len(log.segments) == 1 && log.active != nil {
			if err := log.active.Close(); err != nil {
				return fmt.Errorf("outputlog: %w", err)
			}
			log.active = nil
		}
		if err := os.Remove(oldest.path); err != nil {
			return fmt.Errorf("outputlog: %w", err)
		}
		if log.resumeSegment == oldest {
			log.resumeSegment = nil
		}
		log.bytes -= oldest.bytes
		log.segments = log.segments[1:]
	}
	return nil
}

// ReadAfter returns the entries numbered above after, in order, stopping once
// it has maximumEntries of them or their payloads pass maximumBytes. It always
// returns at least one entry when there is one. Asking for entries the log has
// already dropped is ErrAlreadyDiscarded.
func (log *Log) ReadAfter(after uint64, maximumEntries int, maximumBytes int64) ([]Entry, error) {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	if log.closed {
		return nil, ErrClosed
	}
	if after < log.acknowledged {
		return nil, fmt.Errorf("%w: asked for entries after %d, and everything up to %d is gone", ErrAlreadyDiscarded, after, log.acknowledged)
	}
	if after > log.lastSequence {
		return nil, fmt.Errorf("%w: asked for entries after %d, and the log ends at %d", ErrNeverAppended, after, log.lastSequence)
	}
	if maximumEntries < 1 {
		return nil, fmt.Errorf("outputlog: maximumEntries is %d", maximumEntries)
	}

	var entries []Entry
	var payloadBytes int64
	next := after + 1
	for _, current := range log.segments {
		if current.lastSequence < next {
			continue
		}
		file, err := os.Open(current.path)
		if err != nil {
			return nil, fmt.Errorf("outputlog: %w", err)
		}
		offset := int64(0)
		if log.resumeSegment == current && log.resumeAfter == next-1 {
			offset = log.resumeOffset
			if _, err := file.Seek(offset, io.SeekStart); err != nil {
				file.Close()
				return nil, fmt.Errorf("outputlog: %w", err)
			}
		}
		for offset < current.bytes {
			entry, size, err := readEntry(file)
			if err != nil {
				file.Close()
				return nil, fmt.Errorf("%w: %s at byte %d: %v", ErrCorrupt, current.path, offset, err)
			}
			offset += size
			if entry.Sequence < next {
				continue
			}
			if entry.Sequence != next {
				file.Close()
				return nil, fmt.Errorf("%w: %s holds entry %d where %d belongs", ErrCorrupt, current.path, entry.Sequence, next)
			}
			entries = append(entries, entry)
			payloadBytes += int64(len(entry.Payload))
			next++
			if len(entries) >= maximumEntries || payloadBytes >= maximumBytes {
				log.resumeSegment, log.resumeAfter, log.resumeOffset = current, entry.Sequence, offset
				file.Close()
				return entries, nil
			}
		}
		log.resumeSegment, log.resumeAfter, log.resumeOffset = current, next-1, offset
		file.Close()
	}
	return entries, nil
}

// LastSequence is the number of the newest entry, or of the newest one ever
// appended when all have been acknowledged and dropped. Zero means none yet.
func (log *Log) LastSequence() uint64 {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return log.lastSequence
}

// AcknowledgedThrough is the highest number the server has confirmed storing.
func (log *Log) AcknowledgedThrough() uint64 {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return log.acknowledged
}

// Bytes is how much the log holds on disk now, to compare with MaximumBytes.
func (log *Log) Bytes() int64 {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return log.bytes
}

// Close releases the log's file. The log stays on disk and can be opened again.
func (log *Log) Close() error {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	if log.closed {
		return nil
	}
	log.closed = true
	if log.active != nil {
		err := log.active.Close()
		log.active = nil
		if err != nil {
			return fmt.Errorf("outputlog: %w", err)
		}
	}
	return nil
}

// Delete closes the log and removes its folder. A host calls it once the
// server has acknowledged a spawn's exit, or has said it no longer wants it.
func (log *Log) Delete() error {
	if err := log.Close(); err != nil {
		return err
	}
	if err := os.RemoveAll(log.folder); err != nil {
		return fmt.Errorf("outputlog: %w", err)
	}
	return nil
}
