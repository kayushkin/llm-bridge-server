package server

// Before grant-store records a grant it asks the resource's owner whether the
// resource exists. The standalone store asked this server over HTTP with the
// service token for agents, instances, machines, peer connections and
// operation types, and principal-store for principals. Inside this process
// those owners are this server's own stores, so they are asked directly, and
// so are tool-store, skill-store and reference-store, which also moved into
// this process. The owner that still runs apart (kanban-store) is asked over
// HTTP as before.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/skillstore"
	"github.com/kayushkin/llm-bridge-server/internal/agents/toolstore"
	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// inProcessResourceChecker routes each check to the resource's owner.
type inProcessResourceChecker struct {
	server     *Server
	principals *principalstore.Store
	overHTTP   grantstore.ResourceChecker
}

func (s *Server) grantResourceChecker(principals *principalstore.Store) grantstore.ResourceChecker {
	return inProcessResourceChecker{
		server:     s,
		principals: principals,
		overHTTP: grantstore.NewHTTPResourceChecker(grantstore.ResourceOwners{
			KanbanStoreURL:          s.cfg.KanbanStoreURL,
			KanbanStoreServiceToken: s.cfg.KanbanStoreServiceToken,
		}),
	}
}

// CheckResourceExists answers nil when the owner has the resource, an error
// wrapping grantstore.ErrResourceNotFound when it does not, and any other
// error when it could not say.
func (checker inProcessResourceChecker) CheckResourceExists(ctx context.Context, resourceType, resourceID string) error {
	notFound := func(owner string) error {
		return fmt.Errorf("%w: %s has no %s %q", grantstore.ErrResourceNotFound, owner, resourceType, resourceID)
	}
	s := checker.server
	switch resourceType {
	case grantstore.ResourceTypeTool:
		tools := s.toolStoreForResourceChecks.Load()
		if tools == nil {
			return fmt.Errorf("this server's tool-store module is off, so tool %q cannot be checked", resourceID)
		}
		number, err := strconv.ParseInt(resourceID, 10, 64)
		if err != nil || strconv.FormatInt(number, 10) != resourceID {
			return notFound("tool-store")
		}
		if _, err := tools.GetTool(ctx, number); errors.Is(err, toolstore.ErrNotFound) {
			return notFound("tool-store")
		} else if err != nil {
			return fmt.Errorf("tool-store: reading tool %s: %w", resourceID, err)
		}
		return nil
	case grantstore.ResourceTypeSkill:
		skills := s.skillStoreForResourceChecks.Load()
		if skills == nil {
			return fmt.Errorf("this server's skill-store module is off, so skill %q cannot be checked", resourceID)
		}
		number, err := strconv.ParseInt(resourceID, 10, 64)
		if err != nil || strconv.FormatInt(number, 10) != resourceID {
			return notFound("skill-store")
		}
		if _, err := skills.GetSkill(ctx, number); errors.Is(err, skillstore.ErrNotFound) {
			return notFound("skill-store")
		} else if err != nil {
			return fmt.Errorf("skill-store: reading skill %s: %w", resourceID, err)
		}
		return nil
	case grantstore.ResourceTypeAgent:
		if s.agentStore == nil {
			return fmt.Errorf("this server has no agent-store, so agent %q cannot be checked", resourceID)
		}
		// A grant names agent-store's numeric id; the slug is renameable.
		number, err := strconv.ParseInt(resourceID, 10, 64)
		if err != nil || strconv.FormatInt(number, 10) != resourceID {
			return notFound("agent-store")
		}
		if _, err := s.agentStore.GetAgent(number); errors.Is(err, sql.ErrNoRows) {
			return notFound("agent-store")
		} else if err != nil {
			return fmt.Errorf("agent-store: reading agent %s: %w", resourceID, err)
		}
		return nil
	case grantstore.ResourceTypeInstance:
		if _, err := s.harnessStore.GetInstance(resourceID); errors.Is(err, sql.ErrNoRows) {
			return notFound("harness-store")
		} else if err != nil {
			return fmt.Errorf("harness-store: reading instance %s: %w", resourceID, err)
		}
		return nil
	case grantstore.ResourceTypeMachine:
		if _, err := s.harnessStore.GetMachine(resourceID); errors.Is(err, sql.ErrNoRows) {
			return notFound("harness-store")
		} else if err != nil {
			return fmt.Errorf("harness-store: reading machine %s: %w", resourceID, err)
		}
		return nil
	case grantstore.ResourceTypePeerConnection:
		if _, err := s.store.GetPeerConnection(resourceID); errors.Is(err, store.ErrPeerConnectionNotFound) {
			return notFound("this server")
		} else if err != nil {
			return fmt.Errorf("reading peer connection %s: %w", resourceID, err)
		}
		return nil
	case grantstore.ResourceTypeOperationType:
		if s.operations == nil {
			return fmt.Errorf("this server runs no operations, so operation type %q cannot be checked", resourceID)
		}
		var known []string
		for _, description := range s.operations.Types() {
			if string(description.Type) == resourceID {
				return nil
			}
			known = append(known, string(description.Type))
		}
		return fmt.Errorf("%w: this server runs no operation type %q; it runs %s", grantstore.ErrResourceNotFound, resourceID, strings.Join(known, ", "))
	case grantstore.ResourceTypeReferenceType:
		resolver := s.referenceResolver.Load()
		if resolver == nil {
			return fmt.Errorf("this server's reference-store is off, so reference type %q cannot be checked", resourceID)
		}
		if referencestore.TypeNamed(resourceID) == nil {
			return notFound("reference-store")
		}
		return nil
	case grantstore.ResourceTypeReference:
		resolver := s.referenceResolver.Load()
		if resolver == nil {
			return fmt.Errorf("this server's reference-store is off, so reference %q cannot be checked", resourceID)
		}
		// Asked for nobody, so a record exists whoever may see it; a store
		// that could not say is a failure, never "does not exist".
		results, lookupErrors := resolver.Resolve(ctx, referencestore.Caller{Unchecked: true}, []string{resourceID})
		if len(results[resourceID]) > 0 {
			return nil
		}
		if len(lookupErrors) > 0 {
			return fmt.Errorf("reference-store could not say whether %q names a record: %s: %s", resourceID, lookupErrors[0].Type, lookupErrors[0].Error)
		}
		return notFound("reference-store")
	case grantstore.ResourceTypePrincipal:
		if _, err := checker.principals.Get(ctx, resourceID, false); errors.Is(err, principalstore.ErrNotFound) {
			return notFound("principal-store")
		} else if err != nil {
			return fmt.Errorf("principal-store: reading %s: %w", resourceID, err)
		}
		return nil
	}
	if slices.Contains(grantstore.ResourceTypesCheckedOverHTTP, resourceType) {
		return checker.overHTTP.CheckResourceExists(ctx, resourceType, resourceID)
	}
	return fmt.Errorf("no owner check for resource_type %q: grant-store names %s, and each needs an owner here or in ResourceTypesCheckedOverHTTP",
		resourceType, strings.Join(grantstore.ResourceTypes, ", "))
}
