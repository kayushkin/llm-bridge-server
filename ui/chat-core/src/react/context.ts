import { createContext, useContext } from 'react';
import type { ApiClient } from '../net/ApiClient.js';
import type { NoteboardClient } from '../net/NoteboardClient.js';
import type { ReferenceStoreClient } from '../net/ReferenceStoreClient.js';
import type { SessionCache } from '../cache/SessionCache.js';
import type { SyncEngine } from '../sync/SyncEngine.js';
import type { Prefetcher } from '../boot/Prefetcher.js';
import type { ChatStoreApi } from '../store/ChatStore.js';

/** The wired singletons the hooks read from. Created once by ChatProvider. */
export interface ChatContextValue {
  store: ChatStoreApi;
  api: ApiClient;
  cache: SessionCache;
  sync: SyncEngine;
  prefetcher: Prefetcher;
  /** Noteboard reader for note/todo ref chips, or null when the host did not
   *  configure a `noteboardBasePath`. Null is a real state, not a failure: a
   *  host that mounts the chat without a noteboard proxy has nowhere to resolve
   *  an item id, and the chip must say so rather than spin forever. */
  noteboard: NoteboardClient | null;
  /** reference-store through the bridge, or null when the host did not
   *  configure a `referenceStoreBasePath`. It answers the list of reference
   *  types and looks ids up for the signed-in person. Null means an id with
   *  no chip of its own stays plain text: there is nowhere to ask. */
  references: ReferenceStoreClient | null;
}

export const ChatContext = createContext<ChatContextValue | null>(null);

export function useChatContext(): ChatContextValue {
  const ctx = useContext(ChatContext);
  if (!ctx) {
    throw new Error('chat-core hooks must be used inside <ChatProvider>');
  }
  return ctx;
}
