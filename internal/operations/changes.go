package operations

import "sync"

// ChangeBroadcaster tells subscribers that an operation has new events. It
// carries no event itself: a subscriber reads the events from the store,
// which is the only record of them, so a slow subscriber can miss a signal
// but never an event.
type ChangeBroadcaster struct {
	mutex       sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
}

// NewChangeBroadcaster returns a broadcaster with no subscribers.
func NewChangeBroadcaster() *ChangeBroadcaster {
	return &ChangeBroadcaster{subscribers: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe returns a channel that receives a value after the operation
// changes, and a function that ends the subscription.
func (b *ChangeBroadcaster) Subscribe(operationID string) (<-chan struct{}, func()) {
	changed := make(chan struct{}, 1)
	b.mutex.Lock()
	if b.subscribers[operationID] == nil {
		b.subscribers[operationID] = map[chan struct{}]struct{}{}
	}
	b.subscribers[operationID][changed] = struct{}{}
	b.mutex.Unlock()
	return changed, func() {
		b.mutex.Lock()
		delete(b.subscribers[operationID], changed)
		if len(b.subscribers[operationID]) == 0 {
			delete(b.subscribers, operationID)
		}
		b.mutex.Unlock()
	}
}

// Notify signals every subscriber of operationID without blocking.
func (b *ChangeBroadcaster) Notify(operationID string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	for changed := range b.subscribers[operationID] {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}
