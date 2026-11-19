// Announcements that a chat's brief or loose ends moved, so every surface
// showing briefs re-reads. Fired by the `session_brief` frame on the
// session-list stream and by every local answer or dismissal.

const sessionBriefChangeListeners = new Set<() => void>();

export function subscribeToSessionBriefChanges(listener: () => void): () => void {
  sessionBriefChangeListeners.add(listener);
  return () => {
    sessionBriefChangeListeners.delete(listener);
  };
}

export function announceSessionBriefsChanged(): void {
  for (const listener of sessionBriefChangeListeners) listener();
}
