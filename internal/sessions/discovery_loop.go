package sessions

import (
	"context"
	"fmt"
	"log"
	"time"
)

const discoveryInterval = 5 * time.Second

// StartDiscovery imports immediately and keeps scanning while the server lives.
// The delay starts after each pass completes, so slow scans never overlap.
func (s *Service) StartDiscovery(ctx context.Context) {
	go RunDiscoveryLoop(ctx, discoveryInterval, s.discoverOnce)
}

func RunDiscoveryLoop(ctx context.Context, interval time.Duration, discover func(context.Context) error) {
	for ctx.Err() == nil {
		if err := discover(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[auto-discover] scan failed: %v", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) validateDiscoveryPrincipal(ctx context.Context) error {
	if s.Cfg.DiscoveryPrincipalID == "" {
		return nil
	}
	principal, err := s.PrincipalStore().Get(ctx, s.Cfg.DiscoveryPrincipalID, false)
	if err != nil {
		return fmt.Errorf("discovery owner: %w", err)
	}
	if principal.Kind != "human" || principal.DisabledAt != 0 {
		return fmt.Errorf("discovery owner %q must be an active human", principal.ID)
	}
	return nil
}
