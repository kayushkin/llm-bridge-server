package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/runnerprotocol"
	"github.com/kayushkin/llm-bridge-server/internal/sessionhost/outputlog"
)

// A sequenced session is a spawn the server gave a SpawnID. Every line its
// process writes is numbered and appended to a log on disk, and sent from
// there. The server acknowledges what it has stored and the log drops it. So
// when the connection is lost the output is not: the process can go on, and
// what it wrote meanwhile is sent when the server is back and says where it
// stopped. A spawn with no SpawnID runs the old way and never comes here.

const (
	// sequencedLineMaximumBytes is the longest line read from a process. A
	// longer one is left out and marked, so the lines after it still arrive.
	sequencedLineMaximumBytes = 4 << 20
	// sequencedLogMaximumBytes is how much unacknowledged output one spawn may
	// keep on disk. At the limit the runner stops reading the process, which
	// then blocks on its next write, until the server acknowledges some.
	sequencedLogMaximumBytes = 64 << 20
	// sequencedSendWindow is how many entries may be sent beyond the last
	// acknowledged one. It bounds what the server must hold in memory.
	sequencedSendWindow = 512
	// sequencedExitDrainWait is how long, after the process has ended, the
	// runner goes on reading output a child of it may still hold open.
	sequencedExitDrainWait = 5 * time.Second
)

type sequencedSession struct {
	sessionID string
	spawnID   string
	log       *outputlog.Log
	// changed gets a value when an entry is appended or acknowledged. One
	// slot: a waiter only needs to know something changed since it looked.
	changed chan struct{}

	mutex   sync.Mutex
	process *Subprocess
	// exited is set once the exit entry is in the log.
	exited bool
	// discard is set when the server no longer wants this spawn: nothing more
	// is sent, and the log goes when the process does.
	discard bool
	// stopSending ends the goroutine sending this spawn's entries over the
	// current connection; nil while there is none.
	stopSending context.CancelFunc
}

func (session *sequencedSession) signalChanged() {
	select {
	case session.changed <- struct{}{}:
	default:
	}
}

// sequencedOutputFolder is where this runner keeps its sessions' output logs.
func sequencedOutputFolder() string {
	return filepath.Join(filepath.Dir(configPath()), "session-output")
}

// clearSequencedOutputLeftByAnEarlierRunner removes logs a previous runner
// process left. Its harness processes ended with it, so nothing can be handed
// back; a session the server still counts as running there is one it will find
// missing and start again.
func clearSequencedOutputLeftByAnEarlierRunner() {
	if err := os.RemoveAll(sequencedOutputFolder()); err != nil {
		log.Printf("[runner] ERROR: could not clear old session output in %s: %v", sequencedOutputFolder(), err)
	}
}

// beginSequenced opens the log of a new sequenced spawn and registers it.
func (r *SubprocessRegistry) beginSequenced(sessionID, spawnID string) (*sequencedSession, error) {
	if strings.ContainsAny(sessionID+spawnID, `/\`) || sessionID == "" || spawnID == "" || strings.HasPrefix(sessionID, ".") || strings.HasPrefix(spawnID, ".") {
		return nil, fmt.Errorf("session id %q or spawn id %q cannot name a folder", sessionID, spawnID)
	}
	outputLog, err := outputlog.Open(filepath.Join(r.outputFolder, sessionID, spawnID), outputlog.Options{MaximumBytes: sequencedLogMaximumBytes})
	if err != nil {
		return nil, fmt.Errorf("open the output log: %w", err)
	}
	session := &sequencedSession{sessionID: sessionID, spawnID: spawnID, log: outputLog, changed: make(chan struct{}, 1)}
	r.mu.Lock()
	r.sequenced[sessionID] = session
	r.mu.Unlock()
	return session, nil
}

// forgetSequenced drops a spawn: stops sending, deletes its log and its entry.
func (r *SubprocessRegistry) forgetSequenced(session *sequencedSession) {
	session.mutex.Lock()
	if session.stopSending != nil {
		session.stopSending()
		session.stopSending = nil
	}
	session.mutex.Unlock()
	if err := session.log.Delete(); err != nil {
		log.Printf("[runner] ERROR: could not delete the output log of session %s: %v", session.sessionID, err)
	}
	r.mu.Lock()
	if r.sequenced[session.sessionID] == session {
		delete(r.sequenced, session.sessionID)
	}
	r.mu.Unlock()
}

// appendEntry adds one entry to the spawn's log, waiting while the log is
// full. That wait is what holds the process back: the caller is the one
// reading its output. It reports false when the spawn was discarded meanwhile.
func (session *sequencedSession) appendEntry(kind outputlog.Kind, payload []byte) bool {
	for {
		session.mutex.Lock()
		discarded := session.discard
		session.mutex.Unlock()
		if discarded {
			return false
		}
		_, err := session.log.Append(kind, payload)
		switch {
		case err == nil:
			session.signalChanged()
			return true
		case errors.Is(err, outputlog.ErrFull):
			// Wait for an acknowledgement to make room. The timer is only so
			// a discard that raced past the signal is still noticed.
			select {
			case <-session.changed:
			case <-time.After(time.Second):
			}
		case errors.Is(err, outputlog.ErrClosed):
			return false
		default:
			// The disk refused the entry. Going on would number the next line
			// as if this one had never been, so the spawn ends here, loudly.
			log.Printf("[runner] ERROR: session %s: cannot keep its output (%v); ending its process", session.sessionID, err)
			session.mutex.Lock()
			process := session.process
			session.mutex.Unlock()
			if process != nil && process.cmd.Process != nil {
				process.cmd.Process.Kill()
			}
			return false
		}
	}
}

// pumpSequencedStdout reads the process's output a line at a time into the log.
func (r *SubprocessRegistry) pumpSequencedStdout(session *sequencedSession, stdout io.Reader, done chan<- struct{}) {
	defer close(done)
	reader := bufio.NewReaderSize(stdout, 64<<10)
	var line []byte
	tooLong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > sequencedLineMaximumBytes+2 {
				tooLong = true
				line = line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // the line goes on
		}
		// The line is complete, or the output has ended.
		if tooLong {
			log.Printf("[runner] ERROR: session %s wrote a line over %d bytes; it is left out and marked", session.sessionID, sequencedLineMaximumBytes)
			if !session.appendEntry(outputlog.KindLineTooLong, nil) {
				return
			}
		} else if text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"); text != "" {
			if !session.appendEntry(outputlog.KindOutputLine, []byte(text)) {
				return
			}
		}
		line, tooLong = line[:0], false
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				log.Printf("[runner] stdout read error (session=%s): %v", session.sessionID, err)
			}
			return
		}
	}
}

// pumpSequencedStderr writes the process's error output to this runner's own
// log. It is not sent: with no connection the frames would pile up behind the
// output and stall the process.
func (r *SubprocessRegistry) pumpSequencedStderr(sessionID string, stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			log.Printf("[runner] %s stderr: %s", sessionID, line)
		}
	}
}

// waitSequencedExit waits for the process to end, lets its output be read to
// the end, and then records the exit as the spawn's last entry.
func (r *SubprocessRegistry) waitSequencedExit(session *sequencedSession, process *Subprocess, stdout io.Closer, stdoutDone <-chan struct{}) {
	// Process.Wait, not Cmd.Wait: Cmd.Wait closes the pipes as soon as the
	// process is gone, which can cut off lines still waiting to be read.
	state, err := process.cmd.Process.Wait()
	exit := runnerprotocol.RunnerExit{SpawnID: session.spawnID}
	switch {
	case err != nil:
		exit.Error = err.Error()
	default:
		exit.ExitCode = state.ExitCode()
	}

	select {
	case <-stdoutDone:
	case <-time.After(sequencedExitDrainWait):
		// Something the process started still holds its output open.
		log.Printf("[runner] session %s: output still open %s after the process ended; closing it", session.sessionID, sequencedExitDrainWait)
		stdout.Close()
		<-stdoutDone
	}
	process.stdin.Close()

	r.mu.Lock()
	if r.procs[session.sessionID] == process {
		delete(r.procs, session.sessionID)
	}
	r.mu.Unlock()
	log.Printf("[runner] exit session=%s code=%d err=%q", session.sessionID, exit.ExitCode, exit.Error)

	payload, _ := json.Marshal(exit)
	appended := session.appendEntry(outputlog.KindExit, payload)
	session.mutex.Lock()
	session.exited = appended
	discard := session.discard
	session.mutex.Unlock()
	if discard || !appended {
		r.forgetSequenced(session)
	}
}

// frameOfEntry turns a log entry into the frame that carries it.
func frameOfEntry(session *sequencedSession, entry outputlog.Entry) (*runnerprotocol.RunnerMessage, error) {
	switch entry.Kind {
	case outputlog.KindOutputLine, outputlog.KindLineTooLong:
		return &runnerprotocol.RunnerMessage{
			Type:      runnerprotocol.RunnerMsgStdout,
			SessionID: session.sessionID,
			Stdout: &runnerprotocol.RunnerStdout{
				Data:        string(entry.Payload),
				SpawnID:     session.spawnID,
				Sequence:    entry.Sequence,
				LineTooLong: entry.Kind == outputlog.KindLineTooLong,
			},
		}, nil
	case outputlog.KindExit:
		var exit runnerprotocol.RunnerExit
		if err := json.Unmarshal(entry.Payload, &exit); err != nil {
			return nil, fmt.Errorf("entry %d is an exit that does not read back: %w", entry.Sequence, err)
		}
		exit.SpawnID, exit.Sequence = session.spawnID, entry.Sequence
		return &runnerprotocol.RunnerMessage{Type: runnerprotocol.RunnerMsgExit, SessionID: session.sessionID, Exit: &exit}, nil
	default:
		return nil, fmt.Errorf("entry %d has kind %d, which this runner does not send", entry.Sequence, entry.Kind)
	}
}

// startSending sends the spawn's entries after sentThrough over the current
// connection, in order, until ctx ends. It never runs ahead of the last
// acknowledgement by more than the window.
func (r *SubprocessRegistry) startSending(ctx context.Context, session *sequencedSession, sentThrough uint64) {
	sendContext, stop := context.WithCancel(ctx)
	session.mutex.Lock()
	if session.stopSending != nil {
		session.stopSending()
	}
	session.stopSending = stop
	session.mutex.Unlock()

	go func() {
		defer stop()
		wait := func() bool {
			select {
			case <-session.changed:
				return true
			case <-sendContext.Done():
				return false
			}
		}
		for sendContext.Err() == nil {
			room := int(session.log.AcknowledgedThrough()+sequencedSendWindow) - int(sentThrough)
			if room <= 0 || session.log.LastSequence() == sentThrough {
				if !wait() {
					return
				}
				continue
			}
			entries, err := session.log.ReadAfter(sentThrough, min(room, 64), 256<<10)
			if err != nil {
				if !errors.Is(err, outputlog.ErrClosed) {
					log.Printf("[runner] ERROR: session %s: cannot read its output log after entry %d: %v", session.sessionID, sentThrough, err)
				}
				return
			}
			for _, entry := range entries {
				frame, err := frameOfEntry(session, entry)
				if err != nil {
					log.Printf("[runner] ERROR: session %s: %v", session.sessionID, err)
					return
				}
				select {
				case r.outgoing <- frame:
					sentThrough = entry.Sequence
				case <-sendContext.Done():
					return
				}
			}
		}
	}()
}

// AcknowledgeSequenced applies the server's acknowledgement to a spawn's log.
// When it covers the exit, the spawn is finished and its log is deleted.
func (r *SubprocessRegistry) AcknowledgeSequenced(sessionID, spawnID string, sequence uint64) {
	r.mu.Lock()
	session := r.sequenced[sessionID]
	r.mu.Unlock()
	if session == nil || session.spawnID != spawnID {
		return // an acknowledgement for a spawn already finished or replaced
	}
	if err := session.log.Acknowledge(sequence); err != nil {
		log.Printf("[runner] ERROR: session %s: the server acknowledged entry %d: %v", sessionID, sequence, err)
		return
	}
	session.signalChanged()
	session.mutex.Lock()
	finished := session.exited && sequence >= session.log.LastSequence()
	session.mutex.Unlock()
	if finished {
		r.forgetSequenced(session)
	}
}

// HeldSessions lists the sequenced spawns this runner has, for its Hello.
func (r *SubprocessRegistry) HeldSessions() []runnerprotocol.RunnerHeldSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := make([]runnerprotocol.RunnerHeldSession, 0, len(r.sequenced))
	for _, session := range r.sequenced {
		session.mutex.Lock()
		if !session.discard {
			held = append(held, runnerprotocol.RunnerHeldSession{
				SessionID:    session.sessionID,
				SpawnID:      session.spawnID,
				LastSequence: session.log.LastSequence(),
				Exited:       session.exited,
			})
		}
		session.mutex.Unlock()
	}
	return held
}

// discardSequenced ends a spawn the server does not want: its process is
// killed and its log deleted, at once if the process has already ended.
func (r *SubprocessRegistry) discardSequenced(session *sequencedSession) {
	session.mutex.Lock()
	session.discard = true
	process, exited := session.process, session.exited
	if session.stopSending != nil {
		session.stopSending()
		session.stopSending = nil
	}
	session.mutex.Unlock()
	session.signalChanged()
	if exited || process == nil {
		r.forgetSequenced(session)
		return
	}
	if process.cmd.Process != nil {
		process.cmd.Process.Kill() // waitSequencedExit deletes the log
	}
}

// ApplyWelcome settles every held spawn against the server's answer: the ones
// it adopts are sent again from where it stopped, and the rest are ended.
func (r *SubprocessRegistry) ApplyWelcome(ctx context.Context, welcome *runnerprotocol.RunnerWelcome) {
	r.stopHoldTimer()
	adopted := make(map[string]runnerprotocol.RunnerAdoptSession, len(welcome.Adopt))
	for _, adopt := range welcome.Adopt {
		adopted[adopt.SessionID] = adopt
	}
	r.mu.Lock()
	sessions := make([]*sequencedSession, 0, len(r.sequenced))
	for _, session := range r.sequenced {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()

	for _, session := range sessions {
		adopt, wanted := adopted[session.sessionID]
		if !wanted || adopt.SpawnID != session.spawnID {
			log.Printf("[runner] the server does not take session %s back; ending it", session.sessionID)
			r.discardSequenced(session)
			continue
		}
		// What the server has stored needs keeping no longer. An error here
		// means the two disagree about what was written, and sending on
		// would leave a gap or a repeat the server cannot see.
		if err := session.log.Acknowledge(adopt.ResumeAfterSequence); err != nil {
			log.Printf("[runner] ERROR: session %s: the server says it stored through entry %d: %v; ending it", session.sessionID, adopt.ResumeAfterSequence, err)
			r.discardSequenced(session)
			continue
		}
		if adopt.ResumeAfterSequence < session.log.AcknowledgedThrough() {
			log.Printf("[runner] ERROR: session %s: the server asks for entries after %d, and those up to %d were acknowledged and are gone; ending it", session.sessionID, adopt.ResumeAfterSequence, session.log.AcknowledgedThrough())
			r.discardSequenced(session)
			continue
		}
		log.Printf("[runner] the server takes session %s back; sending from entry %d of %d", session.sessionID, adopt.ResumeAfterSequence+1, session.log.LastSequence())
		session.mutex.Lock()
		finished := session.exited && adopt.ResumeAfterSequence >= session.log.LastSequence()
		session.mutex.Unlock()
		if finished {
			r.forgetSequenced(session)
			continue
		}
		r.startSending(ctx, session, adopt.ResumeAfterSequence)
	}
}

// ConnectionLost is called each time the runner finds itself without a
// connection. Spawns that are not sequenced cannot outlive it and are ended.
// Sequenced ones are ended too unless the server asked for them to be held;
// held, they stop sending and go on running, and after holdFor with still no
// server each is interrupted once, and kept.
func (r *SubprocessRegistry) ConnectionLost(hold bool, holdFor time.Duration) {
	r.mu.Lock()
	sessions := make([]*sequencedSession, 0, len(r.sequenced))
	for _, session := range r.sequenced {
		sessions = append(sessions, session)
	}
	unsequenced := make([]*Subprocess, 0, len(r.procs))
	for sessionID, process := range r.procs {
		if r.sequenced[sessionID] == nil {
			unsequenced = append(unsequenced, process)
		}
	}
	r.mu.Unlock()

	for _, process := range unsequenced {
		if process.cmd.Process != nil {
			process.cmd.Process.Kill()
		}
	}
	for _, session := range sessions {
		if !hold {
			r.discardSequenced(session)
			continue
		}
		session.mutex.Lock()
		if session.stopSending != nil {
			session.stopSending()
			session.stopSending = nil
		}
		session.mutex.Unlock()
	}
	if hold && len(sessions) > 0 {
		r.startHoldTimer(holdFor)
	}
}

// startHoldTimer arms the limit on working with no server, unless it is
// already running: a runner that keeps failing to reconnect must not push the
// limit back each time.
func (r *SubprocessRegistry) startHoldTimer(holdFor time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.holdTimer != nil || holdFor <= 0 {
		return
	}
	log.Printf("[runner] holding %d session(s) with no server; their work is interrupted if none returns within %s", len(r.sequenced), holdFor)
	r.holdTimer = time.AfterFunc(holdFor, func() {
		r.mu.Lock()
		processes := make([]*Subprocess, 0, len(r.sequenced))
		for _, session := range r.sequenced {
			session.mutex.Lock()
			if session.process != nil && !session.exited {
				processes = append(processes, session.process)
			}
			session.mutex.Unlock()
		}
		r.mu.Unlock()
		log.Printf("[runner] no server for %s; interrupting %d held session(s), and keeping their processes", holdFor, len(processes))
		for _, process := range processes {
			if err := process.Signal(runnerprotocol.RunnerSignalInterrupt); err != nil {
				log.Printf("[runner] could not interrupt session %s: %v", process.sessionID, err)
			}
		}
	})
}

func (r *SubprocessRegistry) stopHoldTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.holdTimer != nil {
		r.holdTimer.Stop()
		r.holdTimer = nil
	}
}

// HoldsSequencedSessions reports whether any sequenced spawn is alive or has
// output the server has not acknowledged. A runner must not replace its own
// binary while it does: that would end them.
func (r *SubprocessRegistry) HoldsSequencedSessions() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sequenced) > 0
}
