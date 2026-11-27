package references

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// InProcessServices are the owning services that run in the same process as
// this store. A record of one of their types is read by a Go call to the
// owning store, never by a request: Readers holds, for each such type, the
// functions that read it.
//
// The server that builds the readers decides what each may see. A read made
// for nobody (InProcessAsker with no principal) acts as that server's own
// service; a read for an owner-decided type names the caller, and the owner
// narrows the answer to what the caller may see, as its routes do.
type InProcessServices struct {
	// Services are the services, by the name the type rows use, that run in
	// this process. Every type of one of them must have a reader.
	Services []string
	// Readers maps a type to how it is read in this process.
	Readers map[string]InProcessReader
}

// InProcessAsker is who an in-process read is for. An empty PrincipalID is
// the server's own service acting as itself.
type InProcessAsker struct {
	PrincipalID string
}

// InProcessReader reads one type's records with Go calls to the store that
// owns them. Each answer is the JSON document the store's own route for the
// type answers, so labels, pages and the record passed on read the same.
type InProcessReader struct {
	// Record reads one record: ErrRecordNotFound for none the asker may
	// see, ErrRefusedByOwner when the owner refuses the asker outright.
	Record func(ctx context.Context, id string, asker InProcessAsker) (json.RawMessage, error)
	// Attachment reads a picture of the record, for the server's own
	// service: ErrNoAttachment when there is none.
	Attachment func(ctx context.Context, id string) (contentType string, data []byte, err error)
	// Search answers the records that match query, as the type's Search
	// route answers them.
	Search func(ctx context.Context, query string, asker InProcessAsker) (json.RawMessage, error)
}

// ErrRecordNotFound is an in-process reader's answer for a record that does
// not exist, or that the asker may not see and whose owner answers as for a
// missing one.
var ErrRecordNotFound = errors.New("record not found")

// ErrRefusedByOwner is an in-process reader's answer when the owner refuses
// the asker: the record's route would have answered 403.
var ErrRefusedByOwner = errors.New("refused by the owner")

// runsHere reports whether service runs in this process.
func (services InProcessServices) runsHere(service string) bool {
	return slices.Contains(services.Services, service)
}

// ValidateInProcessReaders refuses a set of readers that leaves a type of an
// in-process service unreadable, or that reads a type no row declares or
// whose service runs elsewhere. A missing reader is a startup error, never a
// request over the network.
func ValidateInProcessReaders(types []ReferenceType, services InProcessServices) error {
	var problems []string
	byType := map[string]*ReferenceType{}
	for index := range types {
		row := &types[index]
		byType[row.Type] = row
		if !services.runsHere(row.Service) {
			continue
		}
		reader := services.Readers[row.Type]
		if row.Get != "" && reader.Record == nil {
			problems = append(problems, fmt.Sprintf("type %q of %s has no in-process record reader", row.Type, row.Service))
		}
		if row.Attachment != "" && reader.Attachment == nil {
			problems = append(problems, fmt.Sprintf("type %q of %s has no in-process attachment reader", row.Type, row.Service))
		}
		if row.Mentionable() && reader.Search == nil {
			problems = append(problems, fmt.Sprintf("type %q of %s has no in-process search", row.Type, row.Service))
		}
	}
	for typeName := range services.Readers {
		row, declared := byType[typeName]
		switch {
		case !declared:
			problems = append(problems, fmt.Sprintf("an in-process reader reads type %q, which no row declares", typeName))
		case !services.runsHere(row.Service):
			problems = append(problems, fmt.Sprintf("an in-process reader reads type %q, whose service %s is not one this process runs", typeName, row.Service))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("reference-store: %s", strings.Join(problems, "; "))
	}
	return nil
}

// inProcessReader is the reader of row's type, and whether its service runs
// in this process. A type of such a service with no reader is an error: the
// validation at start makes that impossible.
func (r *Resolver) inProcessReader(row *ReferenceType) (InProcessReader, bool, error) {
	if !r.InProcess.runsHere(row.Service) {
		return InProcessReader{}, false, nil
	}
	reader, found := r.InProcess.Readers[row.Type]
	if !found {
		return InProcessReader{}, true, fmt.Errorf("type %q of %s, which runs in this process, has no reader", row.Type, row.Service)
	}
	return reader, true, nil
}

// fetchInProcess reads one record with its reader, with the answers fetch
// gives a record read over HTTP.
func fetchInProcess(ctx context.Context, row *ReferenceType, reader InProcessReader, id string, caller Caller, asCaller bool) (json.RawMessage, bool, error) {
	if reader.Record == nil {
		return nil, false, fmt.Errorf("type %q of %s has no record reader", row.Type, row.Service)
	}
	asker := InProcessAsker{}
	if asCaller {
		asker.PrincipalID = caller.PrincipalID
	}
	data, err := reader.Record(ctx, id, asker)
	switch {
	case errors.Is(err, ErrRecordNotFound):
		return nil, false, nil
	case asCaller && errors.Is(err, ErrRefusedByOwner):
		return nil, false, errOwnerRefused
	case err != nil:
		return nil, false, fmt.Errorf("%s: %w", row.Service, err)
	case len(data) > maxRecordSize:
		return nil, false, fmt.Errorf("%s answered more than %d bytes", row.Service, maxRecordSize)
	case !json.Valid(data):
		return nil, false, fmt.Errorf("%s answered something that is not JSON", row.Service)
	}
	return data, true, nil
}
