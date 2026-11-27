// @kayushkin/chat-core — framework-light data layer for dash's chat surface.
// The client renders from memory; the network only reconciles.

// ---- Public React surface (docs/PUBLIC-API.md) ----
export { ChatProvider, type ChatProviderProps } from './react/Provider.js';
export {
  useConnState,
  useSessionList,
  useActiveSession,
  usePendingSession,
  useTurns,
  useFullEntry,
  useComposer,
  type ComposerSendOptions,
  useFilters,
  useSessionActions,
  useFolders,
  useSessionInfo,
  useManagedSession,
  useSessionControls,
  useHarness,
  useHarnessCapabilities,
  useModels,
  useSessionCost,
  useContextUsage,
  usePendingPermissions,
  useBudgetHalt,
  useSpendCeiling,
  useSessionStatus,
  useInProgressTodo,
  usePrefetch,
} from './react/hooks.js';
export {
  useSessionRefDetail,
  useNoteOrIssueRefDetail,
  useResolvedRef,
  useSessionRefTranscript,
  clearRefDetailCache,
  loadSessionRefDetail,
  REF_TRANSCRIPT_TURNS,
  type RefDetailState,
  useFilesNamedByTools,
  useSessionPrincipals,
  useFileNamedByToolsContent,
} from './react/refDetail.js';
export {
  fileMentionOf,
  filesMatchingMention,
  type FileMention,
  type FileNamedByTools,
  type FileNamedByToolsContent,
} from './reduce/fileMentions.js';
export {
  type ResolvedRefMatch,
  type ResolveRefError,
  type ResolveResponse,
  ReferenceStoreClient,
  isResolvableReferenceType,
  type ReferenceStoreClientConfig,
  type ReferenceType,
  type ReferenceSummaryField,
  type ReferenceFieldFormat,
} from './net/ReferenceStoreClient.js';

// The context itself, for a component library that draws on this store —
// bridge-ui's reference chips and signal cards read `api` off it. Every React
// component this package used to ship lives there now; this package is the
// data layer and ships none.
export { ChatContext, useChatContext, type ChatContextValue } from './react/context.js';

// ---- Session briefs (each chat's goal, step, changes and loose ends) ----
export { useSessionBriefs, type SessionBriefsState } from './react/sessionBriefs.js';
export {
  sessionBriefFromWire,
  sessionBriefChanges,
  openLooseEnds,
  type SessionBrief,
  type SessionBriefRevision,
  type LooseEnd,
  type LooseEndKind,
  type LooseEndState,
  type SessionBriefViewWire,
} from './net/sessionBriefs.js';
export { subscribeToSessionBriefChanges, announceSessionBriefsChanged } from './store/sessionBriefChanges.js';

// ---- Session signals (the open questions a session is waiting on) ----
export {
  useOpenSignals,
  useOpenSignalsByAttention,
  clearOpenSignalsCache,
  type OpenSignalsState,
  type OpenSignalsByAttentionState,
} from './react/signals.js';
// Whose a waiting session or signal is to answer: the operator's (interactive,
// herald) or background (everything else, unknown types included). The one rule
// every "needs you" surface uses.
export {
  isOperatorSessionType,
  isAwaitingHumanState,
  isSessionAwaitingOperator,
  sessionTypeOfSignal,
  splitSignalsByAttention,
  type SignalsByAttention,
  type SignalAttentionGroup,
} from './net/attention.js';
export { useSessionNames, clearSessionNameCache } from './react/sessionNames.js';
export {
  acknowledgeSignal,
  answerSignalRequest,
  answerTextOf,
  dismissSignal,
  everyQuestionAnswered,
  questionsIn,
  subscribeToSignalChanges,
} from './store/signalResolve.js';
export {
  groupSignalsByRequest,
  signalFromWire,
  SIGNAL_KIND_NOTIFICATION,
  SIGNAL_KIND_QUESTION,
  SIGNAL_SEVERITY_INFO,
  SIGNAL_SEVERITY_WARN,
  SIGNAL_SOURCE_DERIVED,
  SIGNAL_SOURCE_TOOL,
  SIGNAL_STATE_ACKNOWLEDGED,
  SIGNAL_STATE_ANSWERED,
  SIGNAL_STATE_DISMISSED,
  SIGNAL_STATE_OPEN,
  SIGNAL_SURFACE_CHAT,
  SIGNAL_SURFACE_KANBAN,
  type Signal,
  type SignalAnswer,
  type SignalAnswerDraft,
  type SignalAnswerWire,
  type SignalOption,
  type SignalCustomerReplyDraft,
  type SignalOptionWire,
  type SignalRequest,
  type SignalResolveState,
  type SignalWire,
} from './net/signals.js';

// ---- Wire types (docs/WIRE.md, src/net/types.ts) ----
export type {
  SessionSummary,
  SessionStatus,
  SessionStatusTool,
  SessionStatusSubagent,
  Validator,
  EntrySource,
  EntryKind,
  Role,
  Entry,
  EntryUsage,
  ToolResultImage,
  SessionFile,
  SessionFileGeneration,
  SessionFileSharer,
  SessionAction,
  SessionActionOffer,
  SessionActionInput,
  SessionActionResultFormat,
  SessionActionReview,
  SessionActionState,
  SessionActionType,
  Turn,
  TurnModel,
  TurnAggregates,
  SessionInfo,
  SessionPrincipal,
  ToolInfo,
  McpServerInfo,
  HarnessConfig,
  HarnessConfigCustom,
  SessionPermissionState,
  HarnessMeta,
  PendingHook,
  HookResolveInput,
  ModelOption,
  SessionConfig,
  ManagedSessionDetail,
  SummaryResponse,
  RecentBundleResponse,
  ValidatorsResponse,
  MessagesResponse,
  StreamResumePoint,
  SearchHit,
  SearchHitWire,
  SearchResponse,
} from './net/types.js';
export { HOOK_SOURCE_PERMISSION, HOOK_SOURCE_USER_INPUT } from './net/types.js';
export type {
  WireEvent,
  WireEventData,
  HookEventWire,
  ManagedSessionWire,
  ManagedSessionDetailWire,
  SessionInfoWire,
  ToolInfoWire,
  McpServerInfoWire,
  HarnessConfigWire,
  HarnessConfigCustomWire,
  HarnessInfoWire,
  StoreModelWire,
  SessionListFrame,
} from './net/wireEvents.js';

// ---- Store + selectors ----
export {
  createChatStore,
  DEFAULT_TURN_RETENTION_BYTES,
  DEFAULT_TURN_RETENTION_MIN_SESSIONS,
  EMPTY_FILTER,
  type ChatState,
  type ChatActions,
  type ChatStoreApi,
  type CreateChatStoreOptions,
  type ConnState,
  type FilterState,
  type NewSessionOpts,
  type PendingSession,
  type ContentHits,
  EMPTY_HOOKS,
} from './store/ChatStore.js';
export { pendingSessionConfig } from './store/pendingConfig.js';
export {
  DraftStore,
  boundDrafts,
  defaultDraftStorage,
  DRAFT_STORAGE_KEY,
  DRAFT_RECORD_VERSION,
  MAX_PERSISTED_DRAFTS,
  MAX_DRAFT_AGE_MS,
  type DraftStorageLike,
} from './store/draftStorage.js';
export { defaultWebStorage, type WebStorageLike } from './store/webStorage.js';
export {
  FilterStore,
  boundFilterValues,
  emptyPersistedFilterAxes,
  FILTER_STORAGE_KEY,
  FILTER_RECORD_VERSION,
  MAX_PERSISTED_FILTER_VALUES,
  PERSISTED_FILTER_AXES,
  type PersistedFilterAxis,
  type PersistedFilterAxes,
} from './store/filterStorage.js';
export { changeSessionPermissionState } from './store/permissionState.js';
export { recheckSessionBudgetHalt, setSessionSpendCeiling } from './store/spendCeiling.js';
export { ARCHIVE_FOLDER, setSessionDone } from './store/markDone.js';
export {
  createFolder,
  deleteFolder,
  moveSessionToFolder,
  renameFolder,
  type FolderMutationDeps,
} from './store/folders.js';
export {
  foldHookEvent,
  pendingHookFromWire,
  resolvePendingHook,
  HOOK_PHASE_AWAITING,
  HOOK_PHASE_COMPLETED,
} from './store/pendingHooks.js';
export {
  newerSessionStatus,
  statusFromBareState,
  statusOf,
  withNewestStatus,
} from './store/sessionStatus.js';
export { inProgressTodoFromModel, type InProgressTodo } from './store/inProgressTodo.js';
export { RUNNING_STATES, isRunningState } from './store/sessionStates.js';
// The sessions a session spawned, joined on the server's own parent pointer
// (`managerSessionId`). Reads the store AND the server: the store so a subagent
// spawned a moment ago is there at once, the server because a child is ordered by
// its own recency and is therefore unreachable by paging toward its parent.
export {
  useSubagentSessions,
  compareSubagents,
  clearSubagentCache,
} from './react/subagents.js';
// Tool call/result pairing. Exported for a host that renders tool cards: an
// entry's `toolResult` alone answers "did this call finish?" only for the live
// tail, and reports every cold-loaded call as still running. See toolPairing.ts.
export { toolIdOf, resultedToolIds } from './store/toolPairing.js';
export {
  budgetHaltFromEvent,
  budgetHaltFromRefusal,
  ERR_CODE_BUDGET_EXCEEDED,
  type BudgetHalt,
} from './store/budgetHalt.js';
export {
  visibleSessions,
  visibleCount,
  activeSummary,
  sessionSummaryFor,
  selectSessionStatus,
  turnsFor,
  turnList,
  entriesFor,
  visibleEntryIdsFor,
  sourcesForEntry,
  matchesFilter,
  DEFAULT_HIDDEN_SESSION_TYPES,
  isSpawnedSession,
  selectFacets,
  selectContentSearchReach,
  sessionCost,
  contextUsage,
  harnessCapabilities,
  modelsForHarness,
  selectTimeline,
  type SessionCost,
  type ContextUsage,
  type Facets,
  type ContentSearchReach,
  type FolderGroup,
  type TimelineTone,
  type TimelineItem,
  type TimelineTurnGroup,
  type TimelineView,
} from './store/selectors.js';

// ---- Reduce (pure, ported logic) ----
export {
  applyEvent,
  applyEvents,
  initTailState,
  olderPageCursor,
  type TailState,
} from './reduce/TurnReducer.js';
export {
  annotateOTelDuplicates,
  groupMembers,
  isOTelSourced,
} from './reduce/otelDedup.js';
export { newestSessionActions, sessionActionIdsPlacedInProse } from './reduce/sessionActions.js';
export {
  parseRefChips,
  remarkRefChips,
  type RefChipOptions,
  type RefKind,
  type RefSegment,
} from './reduce/refChips.js';
export { useResolvableIdPatterns, useReferenceTypes } from './react/resolvableIdPatterns.js';

// ---- Cache ----
export {
  SessionCache,
  type HydratedCache,
  type CachedListRow,
} from './cache/SessionCache.js';
export {
  selectEvictions,
  enforceCacheBound,
  enforceListBound,
  DEFAULT_CACHE_LIMIT,
  DEFAULT_LIST_CACHE_LIMIT,
} from './cache/evict.js';

// ---- Sync ----
export { SyncEngine, type SyncEngineConfig } from './sync/SyncEngine.js';
export { connectListSSE, connectSessionSSE, summaryFromManaged } from './sync/sse.js';

// ---- Boot ----
export { Prefetcher, type PrefetcherConfig } from './boot/Prefetcher.js';

// ---- Net ----
export { ApiClient, ApiError, type ApiClientConfig, type CreatedSession, type SendResult } from './net/ApiClient.js';
export {
  NotesAndIssuesClient,
  type NotesAndIssuesClientConfig,
  type NoteOrIssue,
  type NoteOrIssueType,
  type ItemSchedule,
} from './net/NotesAndIssuesClient.js';
