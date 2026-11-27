package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge/msg"

	"github.com/kayushkin/llm-bridge-server/internal/machines/runnerprotocol"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// A sequenced spawn is a harness process on a runner whose output arrives
// numbered. The runner keeps each entry until this server acknowledges it, and
// the server stores an entry's event together with the entry's number. So the
// two always agree on how far the output has been stored, and after either
// side loses the other the runner sends again from exactly there: nothing is
// lost and nothing is stored twice.
//
// That is also what lets a process outlive a restart of this server. The
// runner reports the spawns it kept, and adoptHeldRunnerSessions takes back
// the ones this server still knows, without starting anything.

const (
	// sequencedIncomingBuffer is how many accepted entries may wait to be
	// handled. A runner sends at most runner.sequencedSendWindow (512) beyond
	// the last acknowledgement, so this is never reached by a correct runner.
	sequencedIncomingBuffer = 1024
	// sequencedAcknowledgeEvery is the most entries handled between two
	// acknowledgements when more are waiting. With none waiting, each handled
	// entry is acknowledged at once.
	sequencedAcknowledgeEvery = 64
	// adoptionCleanupWait is how long taking a session back waits for the
	// teardown of the process handle it had on an earlier connection.
	adoptionCleanupWait = 10 * time.Second
	// runnerReturnMargin is added to the hold limit when deciding how long to
	// wait for a runner that may be holding sessions, to cover its reconnect.
	runnerReturnMargin = 2 * time.Minute
)

// sequencedEntry is one entry accepted from a runner, in the order it came.
type sequencedEntry struct {
	sequence uint64
	// event is the entry's line parsed; nil for a line that was left out or
	// did not parse, which is acknowledged and otherwise skipped.
	event *msg.Event
	exit  bool
}

func newRunnerSpawnID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("make a spawn id: %w", err)
	}
	return "spawn_" + hex.EncodeToString(buffer), nil
}

// newSequencedRunnerProcess makes the handle of a sequenced spawn whose
// output this server has stored through storedThrough, and starts handing its
// entries to the manager.
func newSequencedRunnerProcess(sessionID, spawnID string, conn *RunnerConnection, st *store.Store, storedThrough uint64) *RunnerProcess {
	process := &RunnerProcess{
		sessionID:    sessionID,
		conn:         conn,
		events:       make(chan msg.Event), // one event at a time: see handEntriesToTheManager
		spawnID:      spawnID,
		store:        st,
		incoming:     make(chan sequencedEntry, sequencedIncomingBuffer),
		handled:      make(chan struct{}),
		done:         make(chan struct{}),
		lastAccepted: storedThrough,
	}
	go process.handEntriesToTheManager()
	return process
}

func (p *RunnerProcess) sequenced() bool { return p.spawnID != "" }

// acceptSequencedOutput takes one output frame from the runner. Only the next
// entry in order is taken: one already seen is a repeat, and one further on
// means entries between were not sent yet, which the runner will send in
// order. Runs on the connection's reader and never blocks it.
func (p *RunnerProcess) acceptSequencedOutput(frame *runnerprotocol.RunnerStdout) {
	if frame.SpawnID != p.spawnID || frame.Sequence != p.lastAccepted+1 {
		if frame.SpawnID == p.spawnID && frame.Sequence > p.lastAccepted+1 {
			log.Printf("[runner-server] %s/%s: entry %d arrived where %d belongs; passing over it until the runner sends in order", p.conn.MachineName, p.sessionID, frame.Sequence, p.lastAccepted+1)
		}
		return
	}
	entry := sequencedEntry{sequence: frame.Sequence}
	switch {
	case frame.LineTooLong:
		log.Printf("[runner-server] ERROR: %s/%s: entry %d was a line too long for the runner to read; it is missing from the session", p.conn.MachineName, p.sessionID, frame.Sequence)
	default:
		var event msg.Event
		if err := json.Unmarshal([]byte(frame.Data), &event); err != nil {
			log.Printf("[runner-server] %s/%s: parse stdout: %v (line=%q)", p.conn.MachineName, p.sessionID, err, frame.Data)
		} else {
			entry.event = &event
		}
	}
	p.enqueue(entry)
}

// acceptSequencedExit takes the runner's exit frame, the spawn's last entry.
func (p *RunnerProcess) acceptSequencedExit(frame *runnerprotocol.RunnerExit) {
	if frame.SpawnID != p.spawnID || frame.Sequence != p.lastAccepted+1 {
		return
	}
	p.enqueue(sequencedEntry{sequence: frame.Sequence, exit: true})
}

func (p *RunnerProcess) enqueue(entry sequencedEntry) {
	select {
	case p.incoming <- entry:
		p.lastAccepted = entry.sequence
	default:
		// More unacknowledged entries than a runner may send. The entry is
		// not taken, so the count stays true; it is sent again when the
		// runner next reconnects.
		log.Printf("[runner-server] ERROR: %s/%s: the runner sent entry %d with %d already waiting; it is not taken", p.conn.MachineName, p.sessionID, entry.sequence, len(p.incoming))
	}
}

// handEntriesToTheManager gives the manager one event at a time and waits for
// it to finish before giving the next, so that "the entry being handled" is
// never in doubt: the manager stores that entry's number with its event. It
// then acknowledges to the runner. It ends when the exit is reached or the
// session is torn down, and it alone closes the events channel.
func (p *RunnerProcess) handEntriesToTheManager() {
	defer close(p.events)
	sinceAcknowledged := 0
	for {
		var entry sequencedEntry
		select {
		case entry = <-p.incoming:
		case <-p.done:
			return
		}
		if entry.event != nil {
			p.currentSequence.Store(entry.sequence)
			select {
			case p.events <- *entry.event:
			case <-p.done:
				return
			}
			select {
			case <-p.handled:
			case <-p.done:
				return
			}
			p.currentSequence.Store(0)
		}
		// The manager stored the number with the event. This covers the
		// entries that carried no event, and costs nothing for the rest.
		if err := p.store.AdvanceRunnerSpawn(p.sessionID, p.spawnID, entry.sequence); err != nil {
			log.Printf("[runner-server] ERROR: %s/%s: %v; not acknowledging entry %d", p.conn.MachineName, p.sessionID, err, entry.sequence)
			continue
		}
		sinceAcknowledged++
		if entry.exit {
			if err := p.store.EndRunnerSpawn(p.sessionID, p.spawnID); err != nil {
				log.Printf("[runner-server] ERROR: %s/%s: %v", p.conn.MachineName, p.sessionID, err)
			}
			p.acknowledge(entry.sequence)
			p.conn.deregisterSession(p.sessionID)
			return
		}
		if len(p.incoming) == 0 || sinceAcknowledged >= sequencedAcknowledgeEvery {
			p.acknowledge(entry.sequence)
			sinceAcknowledged = 0
		}
	}
}

func (p *RunnerProcess) acknowledge(sequence uint64) {
	err := p.conn.Send(&runnerprotocol.RunnerMessage{
		Type:      runnerprotocol.RunnerMsgAck,
		SessionID: p.sessionID,
		Ack:       &runnerprotocol.RunnerAck{SpawnID: p.spawnID, Sequence: sequence},
	})
	if err != nil {
		// Not fatal: a later acknowledgement covers this one, and a runner
		// that reconnects is told the stored number in the Welcome.
		log.Printf("[runner-server] %s/%s: could not acknowledge entry %d: %v", p.conn.MachineName, p.sessionID, sequence, err)
	}
}

// EventHandled is the manager saying it has finished with the event it was
// last given. For a spawn that is not sequenced it does nothing.
func (p *RunnerProcess) EventHandled() {
	if !p.sequenced() {
		return
	}
	select {
	case p.handled <- struct{}{}:
	case <-p.done:
	}
}

// EntryBeingHandled names the runner spawn and the entry whose event the
// manager is handling now, so the manager can store the two together.
func (p *RunnerProcess) EntryBeingHandled() (spawnID string, sequence uint64, sequenced bool) {
	if !p.sequenced() {
		return "", 0, false
	}
	sequence = p.currentSequence.Load()
	return p.spawnID, sequence, sequence != 0
}

// adoptHeldRunnerSessions settles the spawns a reconnecting runner kept. A
// spawn is taken back when it is still the session's current one, on this
// machine, and the runner has everything after what the server stored. Taking
// it back starts nothing: the session gets a handle on the process it already
// had, and the runner is told from which entry to send again. Anything else
// the runner kept is left out of the answer, which tells the runner to end it.
func (m *Manager) adoptHeldRunnerSessions(connection *RunnerConnection, held []runnerprotocol.RunnerHeldSession) []runnerprotocol.RunnerAdoptSession {
	heldBySession := make(map[string]runnerprotocol.RunnerHeldSession, len(held))
	for _, spawn := range held {
		heldBySession[spawn.SessionID] = spawn
	}

	var adopt []runnerprotocol.RunnerAdoptSession
	for _, heldSpawn := range held {
		recorded, err := m.store.RunnerSpawnOfSession(heldSpawn.SessionID)
		if err != nil {
			log.Printf("[runner-server] ERROR: %s: %v; not taking session %s back", connection.MachineName, err, heldSpawn.SessionID)
			continue
		}
		refusal := ""
		switch {
		case recorded == nil:
			refusal = "this server has no record of a spawn for it"
		case recorded.SpawnID != heldSpawn.SpawnID:
			refusal = "the session has since been started again"
		case recorded.Ended:
			refusal = "this server had already given the spawn up"
		case recorded.MachineID != connection.MachineID:
			refusal = "the spawn was recorded on another machine"
		case heldSpawn.LastSequence < recorded.LastSequence:
			refusal = fmt.Sprintf("the runner's log ends at entry %d and this server stored through %d", heldSpawn.LastSequence, recorded.LastSequence)
		}
		if refusal == "" {
			if _, err := m.store.GetSession(heldSpawn.SessionID); err != nil {
				refusal = fmt.Sprintf("its session cannot be read: %v", err)
			}
		}
		if refusal != "" {
			log.Printf("[runner-server] %s holds session %s, which is not taken back: %s", connection.MachineName, heldSpawn.SessionID, refusal)
			if recorded != nil && recorded.SpawnID == heldSpawn.SpawnID {
				if err := m.store.EndRunnerSpawn(heldSpawn.SessionID, heldSpawn.SpawnID); err != nil {
					log.Printf("[runner-server] ERROR: %v", err)
				}
			}
			continue
		}

		// A handle left from an earlier connection must be gone first: its
		// teardown clears the same per-session records this one is about to
		// fill.
		if err := m.waitForProcessCleanup(heldSpawn.SessionID, adoptionCleanupWait); err != nil {
			log.Printf("[runner-server] ERROR: %s: %v; not taking session %s back", connection.MachineName, err, heldSpawn.SessionID)
			continue
		}

		process := newSequencedRunnerProcess(heldSpawn.SessionID, heldSpawn.SpawnID, connection, m.store, recorded.LastSequence)
		connection.registerSession(process)
		m.mu.Lock()
		m.processes[heldSpawn.SessionID] = process
		m.processStartedAt[heldSpawn.SessionID] = time.Now()
		cleanupDone := m.trackProcessCleanupLocked(heldSpawn.SessionID)
		m.mu.Unlock()
		go m.runProcessCleanupTracked(heldSpawn.SessionID, cleanupDone, func() { m.readEvents(process) })

		log.Printf("[runner-server] took session %s back from %s: stored through entry %d, the runner has through %d%s",
			heldSpawn.SessionID, connection.MachineName, recorded.LastSequence, heldSpawn.LastSequence, map[bool]string{true: ", and its process has ended", false: ""}[heldSpawn.Exited])
		adopt = append(adopt, runnerprotocol.RunnerAdoptSession{SessionID: heldSpawn.SessionID, SpawnID: heldSpawn.SpawnID, ResumeAfterSequence: recorded.LastSequence})
	}

	// A spawn this server counts as running on the machine, which the runner
	// did not report, has no process any more.
	open, err := m.store.OpenRunnerSpawnsOnMachine(connection.MachineID)
	if err != nil {
		log.Printf("[runner-server] ERROR: %v", err)
		return adopt
	}
	for _, spawn := range open {
		if heldSpawn, isHeld := heldBySession[spawn.SessionID]; isHeld && heldSpawn.SpawnID == spawn.SpawnID {
			continue
		}
		if err := m.store.EndRunnerSpawn(spawn.SessionID, spawn.SpawnID); err != nil {
			log.Printf("[runner-server] ERROR: %v", err)
		}
	}
	return adopt
}

// waitForProcessCleanup waits for the teardown of the session's previous
// process handle, if one is still running.
func (m *Manager) waitForProcessCleanup(sessionID string, timeout time.Duration) error {
	m.mu.Lock()
	cleanupDone := m.processCleanups[sessionID]
	m.mu.Unlock()
	if cleanupDone == nil {
		return nil
	}
	select {
	case <-cleanupDone:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("session %s: the teardown of its previous process handle did not finish within %s", sessionID, timeout)
	}
}

// AwaitingRunner reports whether a session with no process here may still
// have one on a runner that is away: its spawn is open, runners are asked to
// hold, the machine's runner is not connected, and it has not been gone
// longer than a held process is allowed to work alone. While it is true the
// session is left as it is; the runner's return settles it.
func (m *Manager) AwaitingRunner(sessionID string) bool {
	if m.runners == nil || m.store == nil {
		return false
	}
	spawn, err := m.store.RunnerSpawnOfSession(sessionID)
	if err != nil || spawn == nil || spawn.Ended {
		return false
	}
	hold, holdFor := m.runners.HoldPolicy()
	if !hold {
		return false
	}
	since, absent := m.runners.MachineHasNoRunnerSince(spawn.MachineID)
	return absent && time.Since(since) < holdFor+runnerReturnMargin
}
