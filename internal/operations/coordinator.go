package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

// IntentError is an intent the coordinator refuses before storing it: the
// caller's mistake, answered with 400 and Code.
type IntentError struct {
	Code    string
	Message string
}

func (e *IntentError) Error() string { return e.Code + ": " + e.Message }

// BudgetError is an intent refused because its organization has spent its
// monthly budget. Answered with 402 and Code.
type BudgetError struct {
	Code    string
	Message string
}

func (e *BudgetError) Error() string { return e.Code + ": " + e.Message }

// Config bounds the coordinator.
type Config struct {
	// WorkerCount is how many operations run at once.
	WorkerCount int
	// LeaseDuration is how long a worker's claim lasts without renewal. A
	// worker renews at a third of it; an operation whose lease runs out is
	// reconciled.
	LeaseDuration time.Duration
	// LeaseOwner names this process. Operations left running under any other
	// owner are reconciled at start.
	LeaseOwner string
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// ModelListPrice returns a model's list price in dollars per million
	// input and output tokens, and whether it has one. Nil knows no prices,
	// so no call under a budget can run.
	ModelListPrice func(model string) (inputPerMillion, outputPerMillion float64, known bool)
}

// Coordinator accepts intents and carries operations to a terminal state.
type Coordinator struct {
	store     *operationstore.Store
	executors map[msg.OperationType]Executor
	config    Config
	changes   *ChangeBroadcaster
	wake      chan struct{}

	attemptsMutex sync.Mutex
	// attemptCancels ends the ctx of each attempt running in this process,
	// by operation id, so a cancel request reaches it at once.
	attemptCancels map[string]context.CancelFunc
}

// NewCoordinator builds a coordinator over store with one executor per type.
// It registers itself as the store's notifier.
func NewCoordinator(store *operationstore.Store, config Config, executors ...Executor) (*Coordinator, error) {
	if config.WorkerCount < 1 {
		return nil, fmt.Errorf("operations: worker count must be at least 1, got %d", config.WorkerCount)
	}
	if config.LeaseDuration < time.Second {
		return nil, fmt.Errorf("operations: lease duration must be at least 1s, got %s", config.LeaseDuration)
	}
	if config.LeaseOwner == "" {
		return nil, errors.New("operations: lease owner is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ModelListPrice == nil {
		config.ModelListPrice = func(string) (float64, float64, bool) { return 0, 0, false }
	}
	byType := map[msg.OperationType]Executor{}
	for _, executor := range executors {
		description := executor.Describe()
		if description.MaximumAttempts < 1 || description.TimeoutSeconds < 1 {
			return nil, fmt.Errorf("operations: executor %s for %s must allow at least one attempt of at least one second", description.Executor, description.Type)
		}
		if _, taken := byType[description.Type]; taken {
			return nil, fmt.Errorf("operations: two executors for type %s", description.Type)
		}
		byType[description.Type] = executor
	}
	coordinator := &Coordinator{
		store:          store,
		executors:      byType,
		config:         config,
		changes:        NewChangeBroadcaster(),
		wake:           make(chan struct{}, 1),
		attemptCancels: map[string]context.CancelFunc{},
	}
	store.SetNotifier(func(event msg.OperationEvent) { coordinator.changes.Notify(event.OperationID) })
	return coordinator, nil
}

// Changes tells subscribers when an operation's events grow.
func (c *Coordinator) Changes() *ChangeBroadcaster { return c.changes }

// Store is the store the coordinator writes.
func (c *Coordinator) Store() *operationstore.Store { return c.store }

// Types describes every operation type this coordinator can run, by type.
func (c *Coordinator) Types() []msg.OperationTypeDescription {
	descriptions := make([]msg.OperationTypeDescription, 0, len(c.executors))
	for _, executor := range c.executors {
		descriptions = append(descriptions, executor.Describe())
	}
	sort.Slice(descriptions, func(i, j int) bool { return descriptions[i].Type < descriptions[j].Type })
	return descriptions
}

// Submit checks an intent and stores it, or returns the operation already
// stored under its idempotency key (created false). The caller must already
// have set PrincipalID from the verified caller and checked the organization.
func (c *Coordinator) Submit(intent msg.OperationIntent) (receipt msg.OperationReceipt, created bool, err error) {
	if intent.ParentOperationID != "" {
		return msg.OperationReceipt{}, false, &IntentError{"parent_not_settable", "parent_operation_id is set by the bridge when an operation starts a child"}
	}
	fingerprint, err := c.checkIntent(intent)
	if err != nil {
		return msg.OperationReceipt{}, false, err
	}
	budget, limited, err := c.store.OrganizationBudget(intent.OrganizationID, c.config.Now())
	if err != nil {
		return msg.OperationReceipt{}, false, err
	}
	if limited && budget.SpentUSD >= budget.MonthlyLimitUSD {
		// A repeat of an intent already accepted goes on to Create, which
		// answers with its receipt (or refuses a changed intent) and stores
		// nothing new: the budget stops new work, not reading about old work.
		_, found, err := c.store.FindByIdempotencyKey(intent)
		if err != nil {
			return msg.OperationReceipt{}, false, err
		}
		if found {
			return c.store.Create(intent, fingerprint, c.config.Now())
		}
		return msg.OperationReceipt{}, false, &BudgetError{"organization_budget_exhausted", fmt.Sprintf(
			"%s has spent $%.4f of its $%.2f budget for the month starting %s", intent.OrganizationID,
			budget.SpentUSD, budget.MonthlyLimitUSD, budget.MonthStartsAt.Format("2006-01-02"))}
	}
	receipt, created, err = c.store.Create(intent, fingerprint, c.config.Now())
	if err != nil {
		return msg.OperationReceipt{}, false, err
	}
	if created {
		c.wakeWorker()
	}
	return receipt, created, nil
}

func (c *Coordinator) checkIntent(intent msg.OperationIntent) (string, error) {
	executor, known := c.executors[intent.Type]
	if !known {
		return "", &IntentError{"unknown_operation_type", fmt.Sprintf("this bridge runs no operation of type %q; GET /operation-types lists the ones it does", intent.Type)}
	}
	if strings.TrimSpace(intent.OrganizationID) == "" {
		return "", &IntentError{"organization_required", "organization_id is required: the principal-store group the operation runs for"}
	}
	if strings.TrimSpace(intent.IdempotencyKey) == "" {
		return "", &IntentError{"idempotency_key_required", "idempotency_key is required; see docs/OPERATIONS.md"}
	}
	if len(intent.RequestedCapabilities) > 0 {
		return "", &IntentError{"unknown_capability", fmt.Sprintf("no capability is defined yet, so %v cannot be granted; leave requested_capabilities empty", intent.RequestedCapabilities)}
	}
	if intent.MaximumCostUSD < 0 {
		return "", &IntentError{"invalid_maximum_cost", "maximum_cost_usd must not be negative"}
	}
	if len(intent.Input) > 0 {
		var input any
		if err := json.Unmarshal(intent.Input, &input); err != nil {
			return "", &IntentError{"input_not_json", "input is not valid JSON: " + err.Error()}
		}
		if path := credentialKeyPath(input, "input"); path != "" {
			return "", &IntentError{"credential_in_input", fmt.Sprintf("%s is named like a credential; an intent must not carry one, and the bridge resolves credentials itself", path)}
		}
	}
	if err := executor.Validate(intent); err != nil {
		return "", &IntentError{"invalid_input", err.Error()}
	}
	return intentFingerprint(intent)
}

// credentialKeyWords are the words that make an input key a credential. A key
// is split on _, -, . and case changes, and matches when any word is here.
var credentialKeyWords = map[string]bool{
	"password": true, "passwd": true, "secret": true, "token": true, "apikey": true,
	"authorization": true, "credential": true, "credentials": true, "cookie": true, "bearer": true,
}

// credentialKeyPath returns the path of the first key in value named like a
// credential, or "".
func credentialKeyPath(value any, path string) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if keyNamesCredential(key) {
				return path + "." + key
			}
			if found := credentialKeyPath(typed[key], path+"."+key); found != "" {
				return found
			}
		}
	case []any:
		for index, element := range typed {
			if found := credentialKeyPath(element, path+"["+strconv.Itoa(index)+"]"); found != "" {
				return found
			}
		}
	}
	return ""
}

func keyNamesCredential(key string) bool {
	words := splitKeyIntoWords(key)
	for index, word := range words {
		if credentialKeyWords[word] {
			return true
		}
		if word == "api" && index+1 < len(words) && words[index+1] == "key" {
			return true
		}
	}
	return false
}

func splitKeyIntoWords(key string) []string {
	var words []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, strings.ToLower(current.String()))
			current.Reset()
		}
	}
	for index, character := range key {
		switch {
		case character == '_' || character == '-' || character == '.' || character == ' ':
			flush()
		case character >= 'A' && character <= 'Z' && index > 0:
			flush()
			current.WriteRune(character)
		default:
			current.WriteRune(character)
		}
	}
	flush()
	return words
}

// intentFingerprint hashes the fields that make two intents "the same" for
// idempotency. Input is parsed and re-encoded, so key order and white space
// do not count.
func intentFingerprint(intent msg.OperationIntent) (string, error) {
	var input any
	if len(intent.Input) > 0 {
		if err := json.Unmarshal(intent.Input, &input); err != nil {
			return "", fmt.Errorf("fingerprint input: %w", err)
		}
	}
	canonical, err := json.Marshal(struct {
		Type                  msg.OperationType        `json:"type"`
		OrganizationID        string                   `json:"organization_id"`
		Scope                 []msg.OperationReference `json:"scope"`
		InputReferences       []msg.OperationReference `json:"input_references"`
		Input                 any                      `json:"input"`
		RequestedCapabilities []string                 `json:"requested_capabilities"`
		PolicyRevision        string                   `json:"policy_revision"`
		MaximumCostUSD        float64                  `json:"maximum_cost_usd"`
	}{intent.Type, intent.OrganizationID, intent.Scope, intent.InputReferences, input, intent.RequestedCapabilities, intent.PolicyRevision, intent.MaximumCostUSD})
	if err != nil {
		return "", fmt.Errorf("fingerprint intent: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Cancel asks an operation to stop. A queued one is cancelled at once; a
// running one is marked and its attempt told to stop. A finished one is
// operationstore.ErrTerminal.
func (c *Coordinator) Cancel(ctx context.Context, operationID string) (msg.OperationReceipt, error) {
	var wasRunning bool
	receipt, err := c.store.Update(operationID, operationstore.AnyLeaseToken, c.config.Now(), func(operation *operationstore.Operation) (msg.OperationEventKind, error) {
		receipt := &operation.Receipt
		if receipt.State == msg.OperationStateQueued {
			receipt.State = msg.OperationStateCancelled
			receipt.Error = &msg.OperationError{Code: "cancelled", Message: "cancelled before it started"}
			return msg.OperationEventCancelled, nil
		}
		wasRunning = true
		if receipt.CancelRequestedAt == nil {
			requestedAt := c.config.Now()
			receipt.CancelRequestedAt = &requestedAt
		}
		return msg.OperationEventCancelRequested, nil
	})
	if err != nil || !wasRunning {
		return receipt, err
	}
	c.attemptsMutex.Lock()
	cancelAttempt := c.attemptCancels[operationID]
	c.attemptsMutex.Unlock()
	if cancelAttempt != nil {
		cancelAttempt()
	}
	if executor, known := c.executors[receipt.Type]; known {
		if err := executor.Cancel(ctx, receipt); err != nil {
			log.Printf("operations: %s: executor could not stop external work on cancel: %v", operationID, err)
		}
	}
	return receipt, nil
}

// Run reconciles what an earlier process left running, then runs workers and
// a watchdog until ctx ends.
func (c *Coordinator) Run(ctx context.Context) {
	c.ReconcileAbandoned(ctx)
	var workers sync.WaitGroup
	for worker := 0; worker < c.config.WorkerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			c.work(ctx)
		}()
	}
	watchdog := time.NewTicker(c.config.LeaseDuration / 2)
	defer watchdog.Stop()
	for {
		select {
		case <-ctx.Done():
			workers.Wait()
			return
		case <-watchdog.C:
			c.ReconcileAbandoned(ctx)
		}
	}
}

func (c *Coordinator) wakeWorker() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coordinator) work(ctx context.Context) {
	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	for ctx.Err() == nil {
		ran, err := c.RunNext(ctx)
		if err != nil {
			log.Printf("operations: claim failed: %v", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-c.wake:
		case <-idle.C:
		}
	}
}

// RunNext claims the oldest queued operation and runs one attempt of it.
// ran is false when nothing was queued.
func (c *Coordinator) RunNext(ctx context.Context) (ran bool, err error) {
	operation, found, err := c.store.Claim(c.config.LeaseOwner, c.config.LeaseDuration, c.config.Now())
	if err != nil || !found {
		return false, err
	}
	// Another worker may be idle while this one runs.
	c.wakeWorker()
	c.runAttempt(ctx, operation)
	return true, nil
}

func (c *Coordinator) runAttempt(ctx context.Context, operation operationstore.Operation) {
	operationID := operation.Receipt.ID
	executor, known := c.executors[operation.Receipt.Type]
	if !known {
		c.settle(operationID, operation.LeaseToken, msg.OperationStateFailed, &msg.OperationError{
			Code: "no_executor", Message: fmt.Sprintf("this bridge no longer runs operations of type %s", operation.Receipt.Type)})
		return
	}
	description := executor.Describe()
	attemptContext, cancelAttempt := context.WithTimeout(ctx, time.Duration(description.TimeoutSeconds)*time.Second)
	defer cancelAttempt()
	c.attemptsMutex.Lock()
	c.attemptCancels[operationID] = cancelAttempt
	c.attemptsMutex.Unlock()
	defer func() {
		c.attemptsMutex.Lock()
		delete(c.attemptCancels, operationID)
		c.attemptsMutex.Unlock()
	}()

	leaseLost := make(chan struct{})
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		ticker := time.NewTicker(c.config.LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-attemptContext.Done():
				return
			case <-ticker.C:
				if err := c.store.RenewLease(operationID, operation.LeaseToken, c.config.Now().Add(c.config.LeaseDuration)); err != nil {
					log.Printf("operations: %s: stopping attempt %d: %v", operationID, operation.Receipt.Attempt, err)
					close(leaseLost)
					cancelAttempt()
					return
				}
			}
		}
	}()

	writer := &leasedReceiptWriter{coordinator: c, operation: operation}
	result := executeRecovering(attemptContext, executor, operation.Intent, writer)
	cancelAttempt()
	<-renewalDone
	select {
	case <-leaseLost:
		// Whoever holds the operation now decides how it ends.
		return
	default:
	}
	c.finishAttempt(operation, description, result)
}

func executeRecovering(ctx context.Context, executor Executor, intent msg.OperationIntent, writer ReceiptWriter) (result Result) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("operations: executor %s panicked on %s: %v\n%s", executor.Describe().Executor, writer.OperationID(), recovered, debug.Stack())
			result = Result{State: msg.OperationStateFailed, Error: &msg.OperationError{
				Code: "executor_panicked", Message: fmt.Sprintf("the %s executor crashed: %v", executor.Describe().Executor, recovered)}}
		}
	}()
	return executor.Execute(ctx, intent, writer)
}

// finishAttempt records how an attempt ended. The rules, in order: an effect
// still in doubt makes the operation unknown whatever the executor said; a
// cancel request ends it cancelled unless the executor succeeded first; a
// retryable error with attempts left queues it again.
func (c *Coordinator) finishAttempt(operation operationstore.Operation, description msg.OperationTypeDescription, result Result) {
	requeued := false
	_, err := c.store.Update(operation.Receipt.ID, operation.LeaseToken, c.config.Now(), func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
		receipt := &stored.Receipt
		receipt.Executor = description.Executor
		if result.Usage != nil {
			receipt.Usage = result.Usage
		}
		state := result.State
		operationError := result.Error
		switch state {
		case "", msg.OperationStateSucceeded, msg.OperationStateFailed, msg.OperationStateConflicted, msg.OperationStateUnknown:
		default:
			operationError = &msg.OperationError{Code: "executor_returned_invalid_state", Message: fmt.Sprintf("the %s executor ended an attempt in state %q", description.Executor, state)}
			state = msg.OperationStateFailed
		}
		if state == "" && operationError == nil {
			operationError = &msg.OperationError{Code: "executor_returned_no_state", Message: fmt.Sprintf("the %s executor ended an attempt without a state or an error", description.Executor)}
			state = msg.OperationStateFailed
		}
		switch {
		case hasEffectInDoubt(receipt.Effects) && state != msg.OperationStateConflicted:
			if state == msg.OperationStateSucceeded {
				log.Printf("operations: %s: executor %s reported success with an effect still unknown; recording unknown", receipt.ID, description.Executor)
			}
			state = msg.OperationStateUnknown
			if operationError == nil || operationError.Code == "" {
				operationError = &msg.OperationError{Code: "effect_outcome_unknown", Message: "an external change was requested and no answer said whether it was applied"}
			}
			operationError.Retryable = false
		case receipt.CancelRequestedAt != nil && state != msg.OperationStateSucceeded:
			state = msg.OperationStateCancelled
			operationError = &msg.OperationError{Code: "cancelled", Message: "cancelled while running"}
		case state == "":
			if operationError.Retryable && receipt.Attempt < description.MaximumAttempts {
				receipt.State = msg.OperationStateQueued
				receipt.Error = operationError
				requeued = true
				return msg.OperationEventQueued, nil
			}
			state = msg.OperationStateFailed
			if operationError.Retryable {
				operationError = &msg.OperationError{Code: operationError.Code, Retryable: true, Message: fmt.Sprintf(
					"%s (gave up after %d attempts)", operationError.Message, receipt.Attempt)}
			}
		}
		receipt.State = state
		receipt.Error = operationError
		if state == msg.OperationStateSucceeded {
			receipt.Error = nil
			receipt.Result = result.Result
			receipt.ResultReferences = result.ResultReferences
		}
		return msg.OperationEventKindForTerminalState[state], nil
	})
	if err != nil {
		log.Printf("operations: %s: could not record the end of attempt %d: %v", operation.Receipt.ID, operation.Receipt.Attempt, err)
		return
	}
	if requeued {
		c.wakeWorker()
	}
}

func hasEffectInDoubt(effects []msg.OperationEffect) bool {
	for _, effect := range effects {
		if effect.Outcome == msg.OperationEffectOutcomeUnknown {
			return true
		}
	}
	return false
}

// settle ends an operation in a terminal state.
func (c *Coordinator) settle(operationID string, leaseToken int64, state msg.OperationState, operationError *msg.OperationError) {
	_, err := c.store.Update(operationID, leaseToken, c.config.Now(), func(operation *operationstore.Operation) (msg.OperationEventKind, error) {
		operation.Receipt.State = state
		operation.Receipt.Error = operationError
		return msg.OperationEventKindForTerminalState[state], nil
	})
	if err != nil {
		log.Printf("operations: %s: could not settle as %s: %v", operationID, state, err)
	}
}

// ReconcileAbandoned settles or requeues every operation left running by a
// process that is gone or a lease that ran out.
func (c *Coordinator) ReconcileAbandoned(ctx context.Context) {
	abandoned, err := c.store.RunningNotHeldBy(c.config.LeaseOwner, c.config.Now())
	if err != nil {
		log.Printf("operations: reconcile: %v", err)
		return
	}
	for _, operation := range abandoned {
		c.reconcileOne(ctx, operation)
	}
}

func (c *Coordinator) reconcileOne(ctx context.Context, operation operationstore.Operation) {
	receipt := operation.Receipt
	executor, known := c.executors[receipt.Type]
	if !known {
		c.settle(receipt.ID, operation.LeaseToken, msg.OperationStateFailed, &msg.OperationError{
			Code: "no_executor", Message: fmt.Sprintf("its worker stopped, and this bridge no longer runs operations of type %s", receipt.Type)})
		return
	}
	description := executor.Describe()
	decision := executor.Reconcile(ctx, receipt)
	var state msg.OperationState
	var operationError *msg.OperationError
	requeue := false
	switch {
	case receipt.CancelRequestedAt != nil && !hasEffectInDoubt(receipt.Effects):
		state, operationError = msg.OperationStateCancelled, &msg.OperationError{Code: "cancelled", Message: "cancelled; its worker stopped before finishing"}
	case decision.Action == ReconcileRunAgain && hasEffectInDoubt(receipt.Effects):
		state, operationError = msg.OperationStateUnknown, &msg.OperationError{Code: "effect_outcome_unknown",
			Message: "its worker stopped after requesting an external change, and no answer said whether it was applied"}
	case decision.Action == ReconcileRunAgain && receipt.Attempt >= description.MaximumAttempts:
		state, operationError = msg.OperationStateFailed, &msg.OperationError{Code: "attempts_exhausted", Retryable: true,
			Message: fmt.Sprintf("its worker stopped on attempt %d of %d", receipt.Attempt, description.MaximumAttempts)}
	case decision.Action == ReconcileRunAgain:
		requeue = true
	case decision.Action == ReconcileSettle && decision.State.IsTerminal():
		state, operationError = decision.State, decision.Error
	default:
		log.Printf("operations: %s: executor %s gave an unusable reconcile decision %+v", receipt.ID, description.Executor, decision)
		state, operationError = msg.OperationStateUnknown, &msg.OperationError{Code: "reconcile_failed",
			Message: "its worker stopped and the bridge could not work out how it ended"}
	}
	_, err := c.store.Update(receipt.ID, operation.LeaseToken, c.config.Now(), func(stored *operationstore.Operation) (msg.OperationEventKind, error) {
		if requeue {
			stored.Receipt.State = msg.OperationStateQueued
			return msg.OperationEventQueued, nil
		}
		stored.Receipt.State = state
		stored.Receipt.Error = operationError
		return msg.OperationEventKindForTerminalState[state], nil
	})
	if err != nil {
		// A worker that renewed in the meantime still holds it; nothing to do.
		log.Printf("operations: %s: reconcile not recorded: %v", receipt.ID, err)
		return
	}
	log.Printf("operations: %s: reconciled after its worker stopped: requeued=%v state=%s", receipt.ID, requeue, state)
	if requeue {
		c.wakeWorker()
	}
}

// leasedReceiptWriter is the ReceiptWriter one attempt gets.
type leasedReceiptWriter struct {
	coordinator *Coordinator
	operation   operationstore.Operation
}

func (w *leasedReceiptWriter) OperationID() string { return w.operation.Receipt.ID }

func (w *leasedReceiptWriter) update(change func(receipt *msg.OperationReceipt) error) error {
	_, err := w.coordinator.store.Update(w.operation.Receipt.ID, w.operation.LeaseToken, w.coordinator.config.Now(),
		func(operation *operationstore.Operation) (msg.OperationEventKind, error) {
			return msg.OperationEventProgress, change(&operation.Receipt)
		})
	return err
}

func (w *leasedReceiptWriter) SetProgress(progress msg.OperationProgress) error {
	return w.update(func(receipt *msg.OperationReceipt) error {
		receipt.Progress = &progress
		return nil
	})
}

func (w *leasedReceiptWriter) AddEvidence(evidence ...msg.OperationEvidence) error {
	return w.update(func(receipt *msg.OperationReceipt) error {
		receipt.Evidence = append(receipt.Evidence, evidence...)
		return nil
	})
}

func (w *leasedReceiptWriter) RecordEffect(target msg.OperationReference, action string) (int, error) {
	var effectNumber int
	err := w.update(func(receipt *msg.OperationReceipt) error {
		receipt.Effects = append(receipt.Effects, msg.OperationEffect{
			Target: target, Action: action, Outcome: msg.OperationEffectOutcomeUnknown, RecordedAt: w.coordinator.config.Now()})
		effectNumber = len(receipt.Effects)
		return nil
	})
	return effectNumber, err
}

func (w *leasedReceiptWriter) SettleEffect(effectNumber int, outcome msg.OperationEffectOutcome, externalOperationID string) error {
	return w.update(func(receipt *msg.OperationReceipt) error {
		if effectNumber < 1 || effectNumber > len(receipt.Effects) {
			return fmt.Errorf("operation %s has no effect %d", receipt.ID, effectNumber)
		}
		effect := &receipt.Effects[effectNumber-1]
		effect.Outcome = outcome
		if externalOperationID != "" {
			effect.ExternalOperationID = externalOperationID
		}
		effect.RecordedAt = w.coordinator.config.Now()
		return nil
	})
}

func (w *leasedReceiptWriter) SpendingAllowance() (float64, bool, error) {
	return w.coordinator.spendingAllowance(w.operation.Receipt)
}

func (w *leasedReceiptWriter) ModelHasListPrice(model string) bool {
	_, _, known := w.coordinator.config.ModelListPrice(model)
	return model != "" && known
}

func (w *leasedReceiptWriter) RecordModelCall(model string, tokens msg.TokenUsage) error {
	call := operationstore.ModelCall{Model: model, Tokens: tokens}
	inputPerMillion, outputPerMillion, known := w.coordinator.config.ModelListPrice(model)
	if known {
		call.CostKnown = true
		call.CostUSD = listPriceUSD(tokens, inputPerMillion, outputPerMillion)
	} else {
		log.Printf("operations: %s: model-store has no price for %q; the call's tokens are recorded and its cost is not", w.operation.Receipt.ID, model)
	}
	_, err := w.coordinator.store.RecordModelCall(w.operation.Receipt.ID, w.operation.LeaseToken, w.coordinator.config.Now(), call)
	return err
}

// listPriceUSD prices a call's tokens. Cache reads and writes are charged at
// the full input price: model-store holds no cache prices, and budgets need
// a figure that errs high rather than low.
func listPriceUSD(tokens msg.TokenUsage, inputPerMillion, outputPerMillion float64) float64 {
	inputTokens := tokens.InputTokens + tokens.CacheReadTokens + tokens.CacheWriteTokens
	return (float64(inputTokens)*inputPerMillion + float64(tokens.OutputTokens)*outputPerMillion) / 1e6
}

// spendingAllowance is what an operation may still spend: the lower of its
// organization's remaining month and its root operation's remaining cap.
// limited is false when neither applies.
func (c *Coordinator) spendingAllowance(receipt msg.OperationReceipt) (remainingUSD float64, limited bool, err error) {
	now := c.config.Now()
	budget, organizationLimited, err := c.store.OrganizationBudget(receipt.OrganizationID, now)
	if err != nil {
		return 0, false, err
	}
	if organizationLimited {
		remainingUSD, limited = budget.MonthlyLimitUSD-budget.SpentUSD, true
	}
	root, err := c.store.RootOf(receipt.ID)
	if err != nil {
		return 0, false, err
	}
	if root.Intent.MaximumCostUSD > 0 {
		spent, err := c.store.TreeSpentUSD(root.Receipt.ID)
		if err != nil {
			return 0, false, err
		}
		if remaining := root.Intent.MaximumCostUSD - spent; !limited || remaining < remainingUSD {
			remainingUSD, limited = remaining, true
		}
	}
	return remainingUSD, limited, nil
}

func (w *leasedReceiptWriter) EffectHeaders(effectNumber int) http.Header {
	receipt := w.operation.Receipt
	return http.Header{
		CorrelationIDHeader:  {receipt.CorrelationID},
		OperationIDHeader:    {receipt.ID},
		IdempotencyKeyHeader: {receipt.ID + ":" + strconv.Itoa(effectNumber)},
	}
}

func (w *leasedReceiptWriter) StartChild(childType msg.OperationType, childKey string, input json.RawMessage, scope []msg.OperationReference) (string, error) {
	parent := w.operation.Receipt
	current, err := w.coordinator.store.Get(parent.ID)
	if err != nil {
		return "", err
	}
	if current.LeaseToken != w.operation.LeaseToken {
		return "", fmt.Errorf("%w: %s", operationstore.ErrLeaseLost, parent.ID)
	}
	intent := msg.OperationIntent{
		Type:              childType,
		OrganizationID:    parent.OrganizationID,
		PrincipalID:       parent.PrincipalID,
		IdempotencyKey:    parent.ID + ":" + childKey,
		CorrelationID:     parent.CorrelationID,
		ParentOperationID: parent.ID,
		Scope:             scope,
		Input:             input,
	}
	fingerprint, err := w.coordinator.checkIntent(intent)
	if err != nil {
		return "", fmt.Errorf("child %s of %s: %w", childKey, parent.ID, err)
	}
	child, created, err := w.coordinator.store.Create(intent, fingerprint, w.coordinator.config.Now())
	if err != nil {
		return "", err
	}
	if created {
		w.coordinator.wakeWorker()
	}
	return child.ID, nil
}
