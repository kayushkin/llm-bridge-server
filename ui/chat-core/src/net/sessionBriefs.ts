// Session briefs — what each chat is for, where it stands, what each turn
// changed, and the loose ends it left: things it said that wanted the person's
// reply and never got one. Canonical Go type: llm-bridge `msg/session_brief.go`;
// written by llm-bridge-server's brief pass at the end of every chat turn.
//
// A loose end is deliberately not a signal. A session holds at most one open
// question signal and any message closes it; a loose end is the remark the
// thread moved past, so it stays open across messages until a later turn
// settles it or the person answers or dismisses it.

export type LooseEndKind = 'question' | 'offer' | 'dropped_problem';
export type LooseEndState = 'open' | 'answered' | 'settled' | 'dismissed';

export interface SessionBriefWire {
  session_id: string;
  overall_goal: string;
  current_step: string;
  revision_count: number;
  created_at: string;
  updated_at: string;
}

export interface SessionBriefRevisionWire {
  id: string;
  session_id: string;
  overall_goal: string;
  current_step: string;
  goal_changed: boolean;
  turn_change?: string;
  created_at: string;
}

export interface LooseEndWire {
  id: string;
  session_id: string;
  kind: LooseEndKind;
  text: string;
  context?: string;
  state: LooseEndState;
  resolution?: string;
  created_at: string;
  resolved_at?: string;
}

export interface SessionBriefViewWire {
  brief: SessionBriefWire | null;
  revisions: SessionBriefRevisionWire[];
  loose_ends: LooseEndWire[];
}

export interface SessionBriefRevision {
  id: string;
  overallGoal: string;
  currentStep: string;
  goalChanged: boolean;
  /** What the turn changed outside the conversation; '' when it only talked. */
  turnChange: string;
  createdAt: string;
}

export interface LooseEnd {
  id: string;
  sessionId: string;
  kind: LooseEndKind;
  text: string;
  context: string;
  state: LooseEndState;
  resolution: string;
  createdAt: string;
  resolvedAt: string | null;
}

/** One chat's brief with its history and loose ends. */
export interface SessionBrief {
  sessionId: string;
  overallGoal: string;
  currentStep: string;
  revisionCount: number;
  createdAt: string;
  updatedAt: string;
  /** Oldest first. */
  revisions: SessionBriefRevision[];
  /** Oldest first; only the states the read asked for. */
  looseEnds: LooseEnd[];
}

/** A view with no brief (a chat no turn has briefed) maps to null. */
export function sessionBriefFromWire(wire: SessionBriefViewWire): SessionBrief | null {
  if (!wire.brief) return null;
  return {
    sessionId: wire.brief.session_id,
    overallGoal: wire.brief.overall_goal,
    currentStep: wire.brief.current_step,
    revisionCount: wire.brief.revision_count,
    createdAt: wire.brief.created_at,
    updatedAt: wire.brief.updated_at,
    revisions: (wire.revisions ?? []).map((revision) => ({
      id: revision.id,
      overallGoal: revision.overall_goal,
      currentStep: revision.current_step,
      goalChanged: revision.goal_changed,
      turnChange: revision.turn_change ?? '',
      createdAt: revision.created_at,
    })),
    looseEnds: (wire.loose_ends ?? []).map((looseEnd) => ({
      id: looseEnd.id,
      sessionId: looseEnd.session_id,
      kind: looseEnd.kind,
      text: looseEnd.text,
      context: looseEnd.context ?? '',
      state: looseEnd.state,
      resolution: looseEnd.resolution ?? '',
      createdAt: looseEnd.created_at,
      resolvedAt: looseEnd.resolved_at ?? null,
    })),
  };
}

/** The revisions that changed something, newest first: a change of goal or a
 *  change outside the conversation. A turn that only talked is left out. */
export function sessionBriefChanges(brief: SessionBrief): SessionBriefRevision[] {
  return brief.revisions.filter((revision) => revision.goalChanged || revision.turnChange !== '').reverse();
}

/** The loose ends still waiting on the person. */
export function openLooseEnds(brief: SessionBrief): LooseEnd[] {
  return brief.looseEnds.filter((looseEnd) => looseEnd.state === 'open');
}
