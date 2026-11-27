// Types — canonical (from llm-bridge)
export type {
  TokenUsage,
  Cost,
  Event,
  InstanceCredential,
  InstanceStatus,
  Machine,
  ManagedSession,
  HarnessInfo,
  HarnessDefaults,
  BridgePrefs,
  MaterializedMessage,
  MaterializedTool,
  ResultEvent,
  BridgeInstance,
  BridgeSession,
  CreateSessionRequest,
} from './types'

// Types — UI-specific
export type {
  FetchFn,
  ToolEvent,
  MessageMeta,
  Message,
  LogRow,
  LogRowActor,
  LogRowKind,
  SessionUIState,
  ActivityKind,
  BridgeEvent,
  EventData,
  UseBridgeSessionReturn,
  BudgetHalt,
} from './types'

// Constants
export { TRANSPORT_LABEL } from './constants'

// Context & Provider
export { BridgeContext, useBridgeConfig, DEFAULT_BRIDGE_ROUTES } from './context'
export type { BridgeConfig, BridgeRoutes, ReferenceChipRenderer, ReferenceChipRendererProps } from './context'
export { BridgeProvider } from './provider'

// Hooks
export { useBridgeAttach } from './useBridgeAttach'
export type {
  AttachStatus,
  AttachRole,
  AttachExit,
  UseBridgeAttachReturn,
  UseBridgeAttachOptions,
} from './useBridgeAttach'
export { useBridgeInstances } from './useBridgeInstances'
export { useInstanceReachable, REACHABILITY_INTERVAL_MS } from './useInstanceReachable'
export { useBridgeMachines } from './useBridgeMachines'
export { useBridgeHarnesses } from './useBridgeHarnesses'
export { useBridgePrefs } from './useBridgePrefs'
export { useBridgeFolders } from './useBridgeFolders'
export type { UseBridgeFoldersReturn } from './useBridgeFolders'
export { useBridgeTools } from './useBridgeTools'
export type { Tool, Kind as ToolKind, MCPSpec, CLISpec, LocalSpec, LocalDescriptor } from '@kayushkin/tool-store-types'
export { useKanban } from './useKanban'
export type { AssignmentOutcome } from './useKanban'
export {
  usePrincipals, pickablePrincipals, principalInitials, principalIsDisabled, indexPrincipalsByID,
} from './usePrincipals'
export type { PrincipalsDirectory, PrincipalKindFilter, PickablePrincipalsFilter } from './usePrincipals'
export type { Principal, Principal as PrincipalDetail, GroupMembership } from '@kayushkin/principal-store-types'
export type { PrincipalKind } from './principalStoreClient'
// The principal-store write client behind the Principals page, for a host that
// wants a principal picker or an inline rename of its own without a second
// copy of the error-text rule. `principalsSearchURL` is the one URL builder.
export {
  principalsSearchURL, principalStoreErrorText, searchPrincipals, getPrincipal, createPrincipal, patchPrincipal,
  setPrincipalDisabled, addGroupMember, removeGroupMember, listPrincipalKinds, PRINCIPALS_SEARCH_LIMIT,
} from './principalStoreClient'
export type {
  PrincipalStoreResult, PrincipalsSearch, CreatePrincipalRequest, PatchPrincipalRequest,
} from './principalStoreClient'
// Grants — who may use what — and where a session runs.
export {
  grantStoreErrorText, grantsListURL, effectiveGrantsURL, listGrantRelations, listGrantResourceTypes, listGrants,
  listEffectiveGrants, createGrant, revokeGrant,
} from './grantStoreClient'
export type { GrantStoreResult, GrantsFilter } from './grantStoreClient'
export type { Grant, RelationDefinition as GrantRelation, GrantRequest as CreateGrantRequest } from '@kayushkin/grant-store-types'
export type { GrantResourceType } from './grantStoreClient'
export {
  resourceTypeWording, relationWording, partitionGrantRows, filterResourceOptions, dispatchInstanceChoices, machineLabel,
} from './grantResources'
export type { ResourceOption, DispatchInstanceChoice, DispatchInstanceChoices } from './grantResources'
export { fetchSessionRunsOn, useSessionRunsOn } from './sessionRunsOn'
export type { SessionRunsOn } from './sessionRunsOn'
export type { Board, Column, Placement, CardLink, CardAssignment, EntityTag, CardView, ColumnView, BoardView, TagCount, ClockState, BusinessHours, BoardPriorityLevel, PriorityLadder, CardEvent, CardNote, CardTimeSummary, TimelineEntry, CardTimeline, Ticket, TicketView, TicketList, TicketLogEntry, EntityCardView } from '@kayushkin/kanban-store-types'
export type { Item as NoteboardItem } from '@kayushkin/noteboard-types'
export { CardBudgetBadge, CardTimelinePanel, describeCardTime, hasClockData } from './components/CardTime'
export { useStickyBottomScroll } from './useStickyBottomScroll'
export type { StickyBottomScroll, StickyBottomScrollOptions } from './useStickyBottomScroll'

// Utils
export { formatTokens, formatCost, formatDuration, timeAgo, formatAgeCompact } from './utils'
export {
  readAgentPrompt, writeAgentPrompt, stripAgentPrompt, suggestAgentPrompt,
  AGENT_PROMPT_OPEN, AGENT_PROMPT_CLOSE,
} from './agentPrompt'

// The whole surface as one component: every page below, the tab row, and both
// providers. A host mounts this at the root of a router and is done.
export { Bridge } from './components/Bridge'
export type { BridgeProps } from './components/Bridge'

// Page components — for a host that composes them by hand instead.
export { BridgeLayout } from './components/BridgeLayout'
export { BridgeEffectiveConfig } from './components/BridgeEffectiveConfig'
export { BridgeHooks } from './components/BridgeHooks'
export { BridgeServiceSettings } from './components/BridgeServiceSettings'
export { BridgeModels } from './components/BridgeModels'
export {
  isModelRoleSetting, roleModelsText, roleOptions, backgroundCallRowsOf, modelPriceText, addableModels, moveModel, removeModel,
  appendModel, roleModelsSaveBody, type RoleOption, type BackgroundCallRow,
} from './modelRoles'
export type { ModelStoreModel, ModelStoreRoles, ModelStoreRoleModelsBody } from './types-model-store'
export { serviceSettingsSourcesOf, serviceSettingsByKind, serviceSettingValueText, serviceSettingsSummary, serviceSettingsURL, serviceSettingURL, fetchServiceSettings, putServiceSetting, type ServiceSettingsSource, type ServiceSettingsGroup, type ServiceSettingsAnswer } from './serviceSettings'
export { BridgeAutoReactions } from './components/messages/BridgeAutoReactions'
export { autoReactionEmojiOf, autoReactionEmojiLabel, accountReactionKeyOf, accountAutoReactionPlaceLabel, AUTO_REACTION_ROUTES_CALLED, type AccountAutoReaction, type CreateAccountAutoReactionBody, type AutoReaction, type DiscordSignupGuild, type DiscordSignupMember, type CreateAutoReactionBody } from './autoReactions'
export { BridgeMessageConversations } from './components/messages/BridgeMessageConversations'
export { BridgeMessageSearch } from './components/messages/BridgeMessageSearch'
export { BridgeMessageContacts } from './components/messages/BridgeMessageContacts'
export { MULTICHAT_ROUTES_CALLED } from './multichatMessages'
export type { MultichatConversation, MultichatMessage, MultichatMessagePage, MultichatSearchAnswer, MultichatTag, MultichatContactTagMap } from './types-multichat'
export { hookWireBodyOf, hookDraftOf, emptyHookDraft, shadowedHookIDs, type HookDraft } from './hookDraft'
export { effectiveConfigQuery, effectiveConfigInputsFromParams, type EffectiveConfigInputs } from './effectiveConfigQuery'
export { SettingsSection, useSectionSave, saveResultOf, SETTINGS_SCOPE_LABEL, SETTINGS_SCOPE_INDEX } from './components/settings/SettingsSection'
export type { SettingsScope, SettingsSectionProps, SaveResult, SectionSave } from './components/settings/SettingsSection'
export { PAGE_GROUPS, BRIDGE_PAGES, bridgePagesShown, navEntriesFor, groupForPath } from './pages'
export type { PageGroupKey, PageGroup, BridgePage, HostPage, NavEntry, ShellFlags } from './pages'
export { BridgeChat } from './components/chat/BridgeChat'
export { BridgeSessions } from './components/BridgeSessions'
export { BridgeInstances } from './components/BridgeInstances'
export { BridgeSettings } from './components/BridgeSettings'
export { BridgeAuth } from './components/BridgeAuth'
export { BridgePeers } from './components/BridgePeers'
export { BridgePeerConnection } from './components/BridgePeerConnection'
export { BridgeUsage } from './components/BridgeUsage'
export { BridgeConformance } from './components/BridgeConformance'
export { BridgeSkills } from './components/BridgeSkills'
export { BridgeTools } from './components/BridgeTools'
export { BridgePermissions } from './components/BridgePermissions'
export { BridgeAgents } from './components/BridgeAgents'
export { BridgeFiles } from './components/BridgeFiles'
export { BridgeKanban } from './components/BridgeKanban'
// The principal-store editor. Renders nothing without `principalsBasePath`,
// and BridgeLayout shows no tab for it then either.
export { BridgePrincipals } from './components/BridgePrincipals'
// bundle-store's curated session bundles, a composer that writes them, and
// the resolve preview. Renders nothing without `bundlesBasePath`, and
// BridgeLayout shows no tab then.
export { BridgeBundles } from './components/BridgeBundles'
export type { Bundle, Member as BundleMember, MemberKind as BundleMemberKind, MemberRef as BundleMemberRef, BundleRef, ResolveResponse as BundleResolution } from '@kayushkin/bundle-store-types'
export type { Repo as RepoStoreRepo } from '@kayushkin/repo-store-types'
export { bundleDraftOf, bundleDraftToWire, emptyBundleDraft, parseTagList } from './bundleDraft'
export type { BundleDraft, BundleDraftMember, BundleDraftResult, BundleWrite } from './bundleDraft'
export { deleteBundle, listBundles, resolveBundles, setBundleEnabled, upsertBundle } from './bundleStoreClient'
export type { BundleStoreResult } from './bundleStoreClient'
// The host's services (healthcheck's list), the SQLite files each holds open,
// their schemas and newest rows — read-only, through the bridge server's
// `/services` routes on `basePath`.
export { BridgeServiceInventory } from './components/BridgeServiceInventory'
export { BridgeMachines } from './components/machines/BridgeMachines'
export { listServices, readDatabaseSchema, readDatabaseRows, rowsQueryString } from './servicesClient'
export type { ServicesResult, RowsQuery } from './servicesClient'
// One card as a page of its own, assembled from its four separately addressable
// parts. `<Bridge>` routes it at `card/:cardId`.
export { BridgeCardPage } from './components/BridgeCardPage'
// The card view, minus the drawer chrome. The board's drawer mounts it inside
// the backdrop; BridgeCardPage inside its own `.bk-drawer` wrapper. Only the
// chrome differs, so only the chrome is repeated.
export { CardDetail } from './components/BridgeKanban'
export type { CardDetailProps } from './components/BridgeKanban'
// The orchestrator's review page — a link to the orchestrator's home session (the
// chat you talk to it in), the week's cost, the injected-context inspector and
// the log of one-shot runs from before the home session.
//
// ⚠️ Needs chat-core's `<ChatProvider>` above it as well as `BridgeProvider`:
// the reference chips' hooks throw without it. `<Bridge>` mounts both; a host
// composing pages by hand must too.
export { BridgeOrchestrator } from './components/BridgeOrchestrator'
export { BridgeSignals } from './components/BridgeSignals'
export { BridgeAttach } from './components/BridgeAttach'
export type { BridgeAttachProps } from './components/BridgeAttach'

// Tool renderers — register custom ones via registerToolRenderer.
// Importing this entrypoint self-registers the five built-ins (Bash, Grep, Web,
// File, and Edit/Write/MultiEdit/NotebookEdit); see components/tools/index.ts.
export { ToolItem, DefaultRenderer, ToolsSection, getToolRenderer, registerToolRenderer } from './components/tools'
// `DiffView` computes a patch from a file's before/after CONTENTS; `UnifiedDiffView`
// colors a diff that already exists (git's own output). They are not interchangeable —
// see the note on `UnifiedDiffView`.
export { DiffView, UnifiedDiffView } from './components/tools'
export type { ToolRendererProps } from './components/tools'

// ToolContext carries the enclosing session id down to the renderers that fetch
// per-tool resources. A host that renders tool cards MUST provide it: the
// EditRenderer and BashRenderer diffs are gated on a non-empty sessionId, and
// the context's default is the empty string — so an unwrapped tool card renders
// its header and silently no diff, with nothing thrown and nothing logged.
export { ToolContext, useToolContext } from './components/tools'

// The auto-grow arithmetic, exported so a host that writes its own composer
// sizes it the same way rather than re-deriving it. Three composers on this
// fleet grow a textarea to its content (this package's, dash's chat page, and
// dash's legacy chat) and two of them shipped the same defect: `scrollHeight`
// excludes the border, so under `box-sizing: border-box` a bare assignment lands
// a border-width short and the box scrolls at every size. Sharing the function
// is what stops the third copy from being written wrong again.
export { composerAutoGrowHeightPx } from './components/chat/composerAutoGrow'

// Shared status dot — used by header, sidebar, and composer status chip
export { StatusDot } from './components/chat/StatusDot'
export type { StatusDotState } from './components/chat/StatusDot'

// Presentation / self-fetching session widgets, for a host that composes its own
// chat surface — dash's chat page is the one that does. Each takes its data via
// props (SessionPermissionMode also reads the public BridgeConfig via
// useBridgeConfig), so none of them assumes a particular layout around it.
export { ToolsPanel } from './components/chat/ToolsPanel'
export type { ToolsPanelProps } from './components/chat/ToolsPanel'
export { SystemPromptModal } from './components/chat/SystemPromptModal'
export type { SystemPromptModalProps } from './components/chat/SystemPromptModal'
export { SessionPermissionMode } from './components/chat/SessionPermissionMode'
export type { SessionPermissionModeProps } from './components/chat/SessionPermissionMode'
export { CostBreakdown } from './components/chat/CostBreakdown'
export type { CostBreakdownProps, CostAggregate } from './components/chat/CostBreakdown'
export type { SpendCeiling } from './components/chat/CostBreakdown'
export { UsageLine } from './components/chat/UsageLine'
export type { UsageLineProps } from './components/chat/UsageLine'
export { EditableName } from './components/chat/EditableName'
export type { EditableNameProps } from './components/chat/EditableName'
// The pinned "Orchestrator" sidebar entry. Its gear panel fetches its settings
// from the host's orchestrator proxy; a click calls `onOpenHomeSession`, which the
// host points at its own session-open path (the chat's sidebar uses
// `useHomeSession`).
export { OrchestratorRow } from './components/chat/OrchestratorRow'
export type { OrchestratorRowProps } from './components/chat/OrchestratorRow'
export { useHomeSession, fetchHomeSessionId } from './components/chat/useHomeSession'
export type { HomeSession } from './components/chat/useHomeSession'

// The three side panes. Each is a self-contained pane with its own header and
// collapse control, so the host that owns the layout decides where the pane goes
// and hands it `style` and `onToggleCollapse`; nothing here assumes any
// particular arrangement around it.
//
// Kanban and Orchestrator fetch their own state from the paths their
// BridgeProvider was given (`issuesBasePath`, `orchestratorBasePath`), and
// render an empty pane when the host left those unset — an unconfigured host is
// a legible state, not an error. GitPanel is the one that takes state in: its
// repo list and selection are shared with the chat's repo dropdown, so the
// caller owns them. See `GitPanelProps`.
export { GitPanel } from './components/GitPanel'
export type { GitPanelProps } from './components/GitPanel'
export type { GitRepo } from './components/GitPanel'
export { LinkedKanbanPanel } from './components/chat/LinkedKanbanPanel'
export type { LinkedKanbanPanelProps } from './components/chat/LinkedKanbanPanel'
export { OrchestratorPanel } from './components/chat/OrchestratorPanel'
export type { OrchestratorPanelProps } from './components/chat/OrchestratorPanel'

// Signals — the questions a session raises. The reads are chat-core's
// (`useOpenSignals`) except the kanban's per-todo pair, which is this library's.
export { useOpenSignalsByTodo, useOpenSignalsForTodo, fetchOpenSignalsByTodo, fetchOpenSignalsForTodo } from './kanbanSignals'
// The cards and chips themselves. On chat-core's store (they read its context),
// styled by this package's `styles.css`; mounted by the chat, the kanban and
// the orchestrator alike.
export { SignalCard, SignalRequestCard } from './components/chat/SignalCard'
export type { SignalCardProps, SignalRequestCardProps } from './components/chat/SignalCard'
export { SessionSignals, SignalRequestList } from './components/chat/SessionSignals'
export type { SessionSignalsProps, SignalRequestListProps } from './components/chat/SessionSignals'
export { RefChip, AnchoredPanel, useAnchoredPanel, RefRow, truncate, idTail, MultiMatchRefChip } from './components/chat/RefChip'
export { GenericReferenceChip, RefusedReferenceChip } from './components/chat/GenericReferenceChip'
export { ReferenceMarkdown, TextWithReferenceChips, TextWithReferenceLinks } from './components/chat/ReferenceText'
export type { ReferenceMarkdownProps } from './components/chat/ReferenceText'
export { EmailRefChip } from './components/chat/EmailRefChip'
export { CardRefChip } from './components/CardRefChip'
export { CardsForEmail } from './components/CardsForEmail'
export { BridgeEmailTickets } from './components/BridgeEmailTickets'
export type { RefChipProps } from './components/chat/RefChip'

// Minimal-chrome (mobile) primitives — auto-engaged below 640px viewport.
// `MinimalChromeProvider` is automatically nested inside `BridgeProvider`,
// so consumers don't need to mount it manually.
//
// The body gets a `bridge-minimal-chrome` class — host apps read it to hide
// their own site chrome via plain CSS — but ONLY once a surface has called
// `useRegisterMinimalChrome(true)` to say it is drawing the replacement top bar
// and drawer. Because the provider rides along with `BridgeProvider`, a narrow
// viewport engages `minimal` on every page the host mounts under one, and most
// of those pages draw no chrome at all; hiding the host's header for them takes
// away the last navigation on the page. A host that ports its own minimal chrome
// calls the hook itself — that is what makes the class true for it.
export { useMinimalChrome, useRegisterMinimalChrome, MinimalChromeProvider, MOBILE_BREAKPOINT } from './components/minimal/MinimalChromeContext'
export type { ChromeOverride } from './components/minimal/MinimalChromeContext'
// The three chrome pieces a host can mount unmodified. Each reads the context above
// and takes no pane vocabulary, so a host with its own set of views still gets the
// same top bar, the same drawer and the same controls sheet rather than a copy that
// drifts from this one.
//
export { MinimalTopBar } from './components/minimal/MinimalTopBar'
export type { MinimalTopBarProps } from './components/minimal/MinimalTopBar'
export { SessionDrawer } from './components/minimal/SessionDrawer'
export { ChromeSheet } from './components/minimal/ChromeSheet'

// The draggable boundary between two panes of a split, and the arithmetic under it.
//
// Exported rather than left internal because its prop list already says it is not
// ours alone: `axis`, a class name, and two accessors. It names no `PaneKey`, reads
// no workspace context and stores nothing — where the panes live and where their
// sizes are kept is entirely the caller's. That parameterization was the point of
// folding the outer and inner resizers into one implementation, and a host with its
// own two-pane split is the third caller it was already shaped for.
//
// `MINIMUM_PANE_PIXELS` comes with it because a caller that clamps its own sizes
// before committing them needs the same number this does; re-deriving it is how the
// two copies that preceded this one drifted.
//
// The handle draws itself with the class the caller passes. `.bc-split-resizer` in
// this package's stylesheet is the styled one; a host loading `styles.css` gets the
// look for free and ships no CSS.
export { SplitDragHandle } from './components/chat/SplitDragHandle'
export type { SplitDragHandleProps, DraggedSplitPair } from './components/chat/SplitDragHandle'
export {
  MINIMUM_PANE_PIXELS,
  EVEN_SPLIT_GROW_UNITS,
  measureSplitDragGeometry,
  splitGrowUnitsAfterDrag,
} from './components/chat/splitDragGeometry'
export type { SplitDragGeometry, SplitGrowUnits } from './components/chat/splitDragGeometry'

// The `?session=<bridge_id>` deeplink reconciler. Pure and dependency-free — no React,
// no router — so any surface that owns its own routing can drive the two-way
// behavior. dash's chat page uses it verbatim rather than growing a second
// implementation that would have to be kept in step with this one. The `awaiting` latch
// in there is the whole reason both directions can coexist; read its header before
// wiring it.
export {
  readSessionDeeplink,
  writeSessionParam,
  initialSessionDeeplinkState,
} from './sessionDeeplink'
export type { SessionDeeplinkState } from './sessionDeeplink'

// The host shell: the theme switch, the top bar's brand and controls, a
// sign-in card, and a sign-in gate for a host that signs people in to
// llm-bridge-server itself (the standalone app). dash draws its own sign-in in
// the same card and uses the rest as they are.
export { BridgeThemeProvider, useBridgeTheme, BRIDGE_THEMES } from './shell/theme'
export type { BridgeTheme, BridgeThemeProviderProps } from './shell/theme'
export { ShellBrand, ShellControls } from './shell/ShellBar'
export type { ShellBrandProps, ShellControlsProps } from './shell/ShellBar'
export { SignInCard, signInCardClassNames } from './shell/SignInCard'
export type { SignInCardProps } from './shell/SignInCard'
export { BridgeSignInGate } from './shell/BridgeSignInGate'
export type { BridgeSignInGateProps, SignedInToBridge } from './shell/BridgeSignInGate'
export {
  bridgeSignInPaths,
  methodNeedsCsrfToken,
  bridgeRefusalText,
  fetchWithBridgeCsrfToken,
  readBridgeSignIn,
  signInWithDemoLogin,
  logOutOfBridge,
} from './bridgeSignIn'
export type { BridgeSignIn } from './bridgeSignIn'
