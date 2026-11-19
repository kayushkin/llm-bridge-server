package outputlog

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openLog(t *testing.T, folder string, options Options) *Log {
	t.Helper()
	log, err := Open(folder, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

func appendLine(t *testing.T, log *Log, text string) uint64 {
	t.Helper()
	sequence, err := log.Append(KindOutputLine, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

func readAll(t *testing.T, log *Log, after uint64) []Entry {
	t.Helper()
	var all []Entry
	for {
		entries, err := log.ReadAfter(after, 7, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			return all
		}
		all = append(all, entries...)
		after = entries[len(entries)-1].Sequence
	}
}

func texts(entries []Entry) []string {
	var lines []string
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("%d:%s", entry.Sequence, entry.Payload))
	}
	return lines
}

func wantTexts(t *testing.T, got []Entry, want ...string) {
	t.Helper()
	if fmt.Sprint(texts(got)) != fmt.Sprint(want) {
		t.Fatalf("read %v, want %v", texts(got), want)
	}
}

var roomy = Options{MaximumBytes: 1 << 20}

func TestEntriesAreNumberedFromOneAndReadBackInOrder(t *testing.T) {
	log := openLog(t, t.TempDir(), roomy)
	for index, line := range []string{"first", "second", "third"} {
		if sequence := appendLine(t, log, line); sequence != uint64(index+1) {
			t.Fatalf("%q was given number %d, want %d", line, sequence, index+1)
		}
	}
	wantTexts(t, readAll(t, log, 0), "1:first", "2:second", "3:third")
	wantTexts(t, readAll(t, log, 2), "3:third")
	wantTexts(t, readAll(t, log, 3))
	if log.LastSequence() != 3 {
		t.Errorf("LastSequence = %d, want 3", log.LastSequence())
	}
}

func TestTheExitEntryKeepsItsPlaceAfterTheLastLine(t *testing.T) {
	log := openLog(t, t.TempDir(), roomy)
	appendLine(t, log, "last words")
	if _, err := log.Append(KindExit, []byte(`{"exit_code":0}`)); err != nil {
		t.Fatal(err)
	}
	entries := readAll(t, log, 0)
	if len(entries) != 2 || entries[0].Kind != KindOutputLine || entries[1].Kind != KindExit || entries[1].Sequence != 2 {
		t.Fatalf("read %+v, want the line and then the exit", entries)
	}
}

func TestAReplayAfterTheServerComesBackStartsWhereItSays(t *testing.T) {
	folder := t.TempDir()
	log := openLog(t, folder, roomy)
	for index := range 10 {
		appendLine(t, log, fmt.Sprintf("line %d", index+1))
	}
	// The server stored 1 to 4 and acknowledged only 1 to 3 before it went.
	if err := log.Acknowledge(3); err != nil {
		t.Fatal(err)
	}
	// Back again, it names 4 as the last it stored.
	replay := readAll(t, log, 4)
	if len(replay) != 6 || replay[0].Sequence != 5 || replay[5].Sequence != 10 {
		t.Fatalf("replayed %v, want 5 to 10", texts(replay))
	}
}

func TestAskingForEntriesTheLogHasDroppedIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	log := openLog(t, t.TempDir(), roomy)
	for range 5 {
		appendLine(t, log, "x")
	}
	if err := log.Acknowledge(4); err != nil {
		t.Fatal(err)
	}
	// A server restored from an old copy, naming 2 as its last.
	_, err := log.ReadAfter(2, 10, 1<<20)
	if !errors.Is(err, ErrAlreadyDiscarded) {
		t.Fatalf("err = %v, want ErrAlreadyDiscarded: an empty answer would hide that entries 3 and 4 are lost to it", err)
	}
}

func TestAnAcknowledgementBeyondTheEndIsRefused(t *testing.T) {
	log := openLog(t, t.TempDir(), roomy)
	appendLine(t, log, "only")
	if err := log.Acknowledge(2); !errors.Is(err, ErrNeverAppended) {
		t.Fatalf("err = %v, want ErrNeverAppended", err)
	}
	if log.AcknowledgedThrough() != 0 {
		t.Errorf("the refused acknowledgement moved the mark to %d", log.AcknowledgedThrough())
	}
}

func TestAnEarlierOrRepeatedAcknowledgementChangesNothing(t *testing.T) {
	log := openLog(t, t.TempDir(), roomy)
	for range 4 {
		appendLine(t, log, "x")
	}
	for _, through := range []uint64{3, 3, 1, 0} {
		if err := log.Acknowledge(through); err != nil {
			t.Fatal(err)
		}
	}
	if log.AcknowledgedThrough() != 3 {
		t.Errorf("AcknowledgedThrough = %d, want 3", log.AcknowledgedThrough())
	}
	wantTexts(t, readAll(t, log, 3), "4:x")
}

func TestAFullLogRefusesTheNextEntryAndTakesItAfterAnAcknowledgement(t *testing.T) {
	line := string(bytes.Repeat([]byte("a"), 100))
	entryBytes := int64(entryOverheadBytes + len(line))
	// Room for four entries, two to a file.
	log := openLog(t, t.TempDir(), Options{MaximumBytes: 4 * entryBytes, SegmentBytes: 2 * entryBytes})
	for range 4 {
		appendLine(t, log, line)
	}
	if _, err := log.Append(KindOutputLine, []byte(line)); !errors.Is(err, ErrFull) {
		t.Fatalf("the fifth entry: err = %v, want ErrFull", err)
	}
	if log.LastSequence() != 4 {
		t.Fatalf("the refused entry moved the numbering to %d", log.LastSequence())
	}
	if err := log.Acknowledge(2); err != nil {
		t.Fatal(err)
	}
	if sequence := appendLine(t, log, line); sequence != 5 {
		t.Fatalf("after the acknowledgement the entry was given %d, want 5: the refused try must not have used a number", sequence)
	}
	entries := readAll(t, log, 2)
	if len(entries) != 3 || entries[0].Sequence != 3 || entries[2].Sequence != 5 {
		t.Fatalf("read %d entries starting at %d, want 3, 4 and 5", len(entries), entries[0].Sequence)
	}
}

func TestAnEntryLargerThanTheWholeAllowanceIsItsOwnError(t *testing.T) {
	log := openLog(t, t.TempDir(), Options{MaximumBytes: 64})
	_, err := log.Append(KindOutputLine, bytes.Repeat([]byte("a"), 100))
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("err = %v, want ErrEntryTooLarge: waiting for an acknowledgement would never make room", err)
	}
}

func TestAcknowledgedFilesAreDeletedAndTheSizeFalls(t *testing.T) {
	folder := t.TempDir()
	line := string(bytes.Repeat([]byte("a"), 100))
	entryBytes := int64(entryOverheadBytes + len(line))
	log := openLog(t, folder, Options{MaximumBytes: 100 * entryBytes, SegmentBytes: 2 * entryBytes})
	for range 6 {
		appendLine(t, log, line)
	}
	files := func() int {
		names, _ := filepath.Glob(filepath.Join(folder, "*"+segmentFileSuffix))
		return len(names)
	}
	if files() != 3 {
		t.Fatalf("six entries at two to a file made %d files, want 3", files())
	}
	if err := log.Acknowledge(5); err != nil {
		t.Fatal(err)
	}
	if files() != 1 || log.Bytes() != 2*entryBytes {
		t.Errorf("after acknowledging 5 of 6: %d files and %d bytes, want 1 file and %d bytes", files(), log.Bytes(), 2*entryBytes)
	}
	if err := log.Acknowledge(6); err != nil {
		t.Fatal(err)
	}
	if files() != 0 || log.Bytes() != 0 {
		t.Errorf("after acknowledging everything: %d files and %d bytes, want none", files(), log.Bytes())
	}
	if sequence := appendLine(t, log, "after"); sequence != 7 {
		t.Errorf("the entry after an emptied log was given %d, want 7", sequence)
	}
}

func TestAReopenedLogGoesOnWhereItStopped(t *testing.T) {
	folder := t.TempDir()
	log := openLog(t, folder, Options{MaximumBytes: 1 << 20, SegmentBytes: 40})
	for index := range 9 {
		appendLine(t, log, fmt.Sprintf("line %d", index+1))
	}
	if err := log.Acknowledge(4); err != nil {
		t.Fatal(err)
	}
	log.Close()

	reopened := openLog(t, folder, Options{MaximumBytes: 1 << 20, SegmentBytes: 40})
	if reopened.LastSequence() != 9 || reopened.AcknowledgedThrough() != 4 {
		t.Fatalf("reopened at last %d, acknowledged %d; want 9 and 4", reopened.LastSequence(), reopened.AcknowledgedThrough())
	}
	entries := readAll(t, reopened, 4)
	if len(entries) != 5 || string(entries[0].Payload) != "line 5" || string(entries[4].Payload) != "line 9" {
		t.Fatalf("read %v after reopening, want lines 5 to 9", texts(entries))
	}
	if sequence := appendLine(t, reopened, "line 10"); sequence != 10 {
		t.Errorf("the first entry after reopening was given %d, want 10", sequence)
	}
}

func TestAReopenedLogThatWasWhollyAcknowledgedKeepsItsNumbering(t *testing.T) {
	folder := t.TempDir()
	log := openLog(t, folder, roomy)
	for range 3 {
		appendLine(t, log, "x")
	}
	if err := log.Acknowledge(3); err != nil {
		t.Fatal(err)
	}
	log.Close()
	reopened := openLog(t, folder, roomy)
	// No file is left to read the last number from; the mark carries it.
	if sequence := appendLine(t, reopened, "y"); sequence != 4 {
		t.Fatalf("the entry was given %d, want 4: number 1 was handed out before and must not be again", sequence)
	}
}

func lastSegmentPath(t *testing.T, folder string) string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(folder, "*"+segmentFileSuffix))
	if err != nil || len(names) == 0 {
		t.Fatalf("no segment files in %s (%v)", folder, err)
	}
	return names[len(names)-1]
}

func TestAHalfWrittenLastEntryIsCutOffOnReopen(t *testing.T) {
	folder := t.TempDir()
	log := openLog(t, folder, roomy)
	appendLine(t, log, "whole one")
	appendLine(t, log, "whole two")
	log.Close()

	// The host died part-way through writing entry 3.
	path := lastSegmentPath(t, folder)
	partial := encodeEntry(3, KindOutputLine, []byte("never finished"))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	file.Write(partial[:len(partial)-5])
	file.Close()

	reopened := openLog(t, folder, roomy)
	wantTexts(t, readAll(t, reopened, 0), "1:whole one", "2:whole two")
	if sequence := appendLine(t, reopened, "written again"); sequence != 3 {
		t.Fatalf("the entry after the cut was given %d, want 3", sequence)
	}
	wantTexts(t, readAll(t, reopened, 0), "1:whole one", "2:whole two", "3:written again")
}

func TestDamageBeforeTheLastFileIsReportedNotCutOff(t *testing.T) {
	folder := t.TempDir()
	log := openLog(t, folder, Options{MaximumBytes: 1 << 20, SegmentBytes: 30})
	for range 6 {
		appendLine(t, log, "a line of text")
	}
	log.Close()

	names, _ := filepath.Glob(filepath.Join(folder, "*"+segmentFileSuffix))
	if len(names) < 3 {
		t.Fatalf("want at least three files to damage a middle one, have %d", len(names))
	}
	contents, err := os.ReadFile(names[1])
	if err != nil {
		t.Fatal(err)
	}
	contents[len(contents)-7] ^= 0xff // one flipped byte inside an entry
	if err := os.WriteFile(names[1], contents, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(folder, roomy); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt: cutting the log here would silently lose every entry after the damage", err)
	}
	// Refusing is not enough: the damaged file must be left as it was found,
	// so that whoever looks into it still has every byte.
	after, err := os.ReadFile(names[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, contents) {
		t.Errorf("opening the damaged log changed the damaged file: %d bytes before, %d after", len(contents), len(after))
	}
}

func TestAMissingFileInTheMiddleIsReported(t *testing.T) {
	folder := t.TempDir()
	log := openLog(t, folder, Options{MaximumBytes: 1 << 20, SegmentBytes: 30})
	for range 6 {
		appendLine(t, log, "a line of text")
	}
	log.Close()
	names, _ := filepath.Glob(filepath.Join(folder, "*"+segmentFileSuffix))
	if err := os.Remove(names[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(folder, roomy); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestReadAfterStopsAtItsLimitsButAlwaysReturnsOne(t *testing.T) {
	log := openLog(t, t.TempDir(), roomy)
	for range 10 {
		appendLine(t, log, "0123456789")
	}
	byCount, err := log.ReadAfter(0, 3, 1<<20)
	if err != nil || len(byCount) != 3 {
		t.Fatalf("with a limit of 3 entries read %d (err %v)", len(byCount), err)
	}
	bySize, err := log.ReadAfter(0, 100, 25)
	if err != nil || len(bySize) != 3 {
		t.Fatalf("with a limit of 25 payload bytes read %d entries of 10 bytes (err %v), want 3: it stops once the limit is passed", len(bySize), err)
	}
	one, err := log.ReadAfter(0, 100, 1)
	if err != nil || len(one) != 1 {
		t.Fatalf("with a limit smaller than one entry read %d (err %v), want 1: a reader must always make progress", len(one), err)
	}
}

func TestALineOfFourMebibytesGoesInAndComesBackWhole(t *testing.T) {
	log := openLog(t, t.TempDir(), Options{MaximumBytes: 16 << 20})
	line := make([]byte, 4<<20)
	for index := range line {
		line[index] = byte(index * 31)
	}
	if _, err := log.Append(KindOutputLine, line); err != nil {
		t.Fatal(err)
	}
	entries := readAll(t, log, 0)
	if len(entries) != 1 || !bytes.Equal(entries[0].Payload, line) {
		t.Fatal("the large line did not read back as it was written")
	}
}

func TestDeleteRemovesTheFolder(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "spawn")
	log := openLog(t, folder, roomy)
	appendLine(t, log, "x")
	if err := log.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Errorf("the folder is still there after Delete (%v)", err)
	}
	if _, err := log.Append(KindOutputLine, []byte("y")); !errors.Is(err, ErrClosed) {
		t.Errorf("appending after Delete: err = %v, want ErrClosed", err)
	}
}

// One goroutine appends as a harness would while another sends and
// acknowledges as the connection would. Every entry must be read exactly once,
// in order, whatever the interleaving. Run with -race.
func TestAppendingWhileSendingAndAcknowledging(t *testing.T) {
	const total = 3000
	log := openLog(t, t.TempDir(), Options{MaximumBytes: 1 << 20, SegmentBytes: 512})
	var appender sync.WaitGroup
	appender.Add(1)
	go func() {
		defer appender.Done()
		for index := 0; index < total; {
			if _, err := log.Append(KindOutputLine, []byte(fmt.Sprintf("line %d", index+1))); err != nil {
				if errors.Is(err, ErrFull) {
					continue // the reader below acknowledges and makes room
				}
				t.Errorf("append: %v", err)
				return
			}
			index++
		}
	}()

	var sent uint64
	for sent < total {
		entries, err := log.ReadAfter(sent, 50, 4096)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Sequence != sent+1 || string(entry.Payload) != fmt.Sprintf("line %d", sent+1) {
				t.Fatalf("read entry %d %q where %d belongs", entry.Sequence, entry.Payload, sent+1)
			}
			sent++
		}
		if len(entries) > 0 {
			if err := log.Acknowledge(sent); err != nil {
				t.Fatal(err)
			}
		}
	}
	appender.Wait()
}

// A long random run of appends, acknowledgements, reads and reopenings,
// checked step by step against a plain slice that does the same thing.
func TestRandomRunsMatchAPlainListOfEntries(t *testing.T) {
	for seed := uint64(1); seed <= 25; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			random := rand.New(rand.NewPCG(seed, seed*7919))
			folder := t.TempDir()
			options := Options{MaximumBytes: 4096, SegmentBytes: int64(64 + random.IntN(400))}
			log := openLog(t, folder, options)

			var model []string // model[i] is entry i+1
			var acknowledged uint64
			for step := range 600 {
				switch action := random.IntN(100); {
				case action < 55:
					line := fmt.Sprintf("entry %d %s", len(model)+1, bytes.Repeat([]byte("."), random.IntN(60)))
					sequence, err := log.Append(KindOutputLine, []byte(line))
					if errors.Is(err, ErrFull) {
						if log.Bytes()+int64(entryOverheadBytes+len(line)) <= options.MaximumBytes {
							t.Fatalf("step %d: refused as full with room to spare", step)
						}
						continue
					}
					if err != nil {
						t.Fatalf("step %d: %v", step, err)
					}
					model = append(model, line)
					if sequence != uint64(len(model)) {
						t.Fatalf("step %d: entry given number %d, want %d", step, sequence, len(model))
					}
				case action < 75:
					if uint64(len(model)) == acknowledged {
						continue
					}
					acknowledged += 1 + uint64(random.IntN(len(model)-int(acknowledged)))
					if err := log.Acknowledge(acknowledged); err != nil {
						t.Fatalf("step %d: %v", step, err)
					}
				case action < 92:
					after := acknowledged + uint64(random.IntN(len(model)-int(acknowledged)+1))
					entries, err := log.ReadAfter(after, 1+random.IntN(8), int64(1+random.IntN(300)))
					if err != nil {
						t.Fatalf("step %d: reading after %d: %v", step, after, err)
					}
					if after < uint64(len(model)) && len(entries) == 0 {
						t.Fatalf("step %d: nothing read after %d with the log ending at %d", step, after, len(model))
					}
					for index, entry := range entries {
						want := after + uint64(index) + 1
						if entry.Sequence != want || string(entry.Payload) != model[want-1] {
							t.Fatalf("step %d: read entry %d %q where %d %q belongs", step, entry.Sequence, entry.Payload, want, model[want-1])
						}
					}
				default:
					log.Close()
					reopened, err := Open(folder, options)
					if err != nil {
						t.Fatalf("step %d: reopening: %v", step, err)
					}
					log = reopened
					t.Cleanup(func() { reopened.Close() })
					if log.LastSequence() != uint64(len(model)) || log.AcknowledgedThrough() != acknowledged {
						t.Fatalf("step %d: reopened at last %d, acknowledged %d; want %d and %d", step, log.LastSequence(), log.AcknowledgedThrough(), len(model), acknowledged)
					}
				}
			}
			// Everything not acknowledged is still there, whole and in order.
			remaining := readAll(t, log, acknowledged)
			if len(remaining) != len(model)-int(acknowledged) {
				t.Fatalf("at the end %d entries remain, want %d", len(remaining), len(model)-int(acknowledged))
			}
		})
	}
}
