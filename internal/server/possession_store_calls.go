package server

// possession-store's calls to the stores in this process: photo-store,
// location-store, mailstack and budget-store, each called in Go through the
// interface possession-store reads it by. Each call acts for the owner the
// context's ownership.Caller names, which possession-store set to its
// request's caller or to the owner it acts for. photo-store and
// location-store are read in that caller's read scope, as their routes read
// it; mailstack and budget-store act only for one principal, so a context
// naming none is refused, and an administrator's sight of every owner is not
// passed on to them.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/budget"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locations"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photos"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/mail"
)

// possessionReadScope is the read scope of the caller possession-store put
// in ctx, as ownership.ReadScope gives a route that request's scope. A
// context with no caller is refused: possession-store always names one.
func possessionReadScope(ctx context.Context, storeName string) (ownership.Scope, error) {
	caller, ok := ownership.CallerFromContext(ctx)
	if !ok {
		return ownership.Scope{}, fmt.Errorf("possession-store asked %s with no caller in its context", storeName)
	}
	scope, refusal := ownership.ReadScopeOf(caller)
	if refusal != nil {
		return ownership.Scope{}, fmt.Errorf("%s refused (%d): %s", storeName, refusal.Status, refusal.Message)
	}
	return scope, nil
}

// possessionOwner is the one principal ctx's caller names, for a store that
// acts only for one owner.
func possessionOwner(ctx context.Context, storeName string) (string, error) {
	caller, ok := ownership.CallerFromContext(ctx)
	if !ok || caller.PrincipalID == "" {
		return "", fmt.Errorf("possession-store asks %s only as one owner, and the call names none", storeName)
	}
	return caller.PrincipalID, nil
}

// possessionPhotoStore is photo-store as possession-store reads it.
type possessionPhotoStore struct {
	server *Server
}

var (
	_ possessions.PhotoStoreReads = possessionPhotoStore{}
	_ possessions.RecordFinder    = possessionPhotoStore{}
)

func (photos possessionPhotoStore) store(ctx context.Context) (*photos.Store, ownership.Scope, error) {
	stores := photos.server.photosAndLocations.Load()
	if stores == nil {
		return nil, ownership.Scope{}, errors.New("photo-store is not open on this server")
	}
	scope, err := possessionReadScope(ctx, "photo-store")
	return stores.photos, scope, err
}

// RecordExists is GET /photos/{id} answering 200 rather than 404.
func (photoStore possessionPhotoStore) RecordExists(ctx context.Context, id string) (bool, error) {
	store, scope, err := photoStore.store(ctx)
	if err != nil {
		return false, err
	}
	_, err = store.Get(ctx, scope, id, false)
	if errors.Is(err, photos.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (photos possessionPhotoStore) Photo(ctx context.Context, photoID string) (possessions.PhotoFacts, error) {
	store, scope, err := photos.store(ctx)
	if err != nil {
		return possessions.PhotoFacts{}, err
	}
	photo, err := store.Get(ctx, scope, photoID, false)
	if err != nil {
		return possessions.PhotoFacts{}, fmt.Errorf("photo-store, reading %s: %w", photoID, err)
	}
	return possessions.PhotoFacts{OwnerPrincipalID: photo.OwnerPrincipalID, Sha256: photo.Sha256, TakenAt: photo.TakenAt,
		SourcePath: photo.SourcePath, Tags: photo.Tags}, nil
}

func (photos possessionPhotoStore) Thumbnail(ctx context.Context, photoID string) ([]byte, error) {
	store, scope, err := photos.store(ctx)
	if err != nil {
		return nil, err
	}
	content, err := store.OpenThumbnail(ctx, scope, photoID)
	if err != nil {
		return nil, fmt.Errorf("photo-store, the thumbnail of %s: %w", photoID, err)
	}
	defer content.Close()
	thumbnail, err := io.ReadAll(content)
	if err != nil {
		return nil, fmt.Errorf("photo-store, reading the thumbnail of %s: %w", photoID, err)
	}
	return thumbnail, nil
}

func (photoStore possessionPhotoStore) ConfirmedFacesOfPerson(ctx context.Context, photoID, personID string) ([]string, error) {
	store, scope, err := photoStore.store(ctx)
	if err != nil {
		return nil, err
	}
	links, err := store.PhotoPeople(ctx, scope, photos.PhotoPersonFilter{PhotoID: photoID, PersonID: personID, Status: "confirmed"})
	if err != nil {
		return nil, fmt.Errorf("photo-store, the people on %s: %w", photoID, err)
	}
	faceIDs := make([]string, 0, len(links))
	for _, link := range links {
		faceIDs = append(faceIDs, link.FaceID)
	}
	return faceIDs, nil
}

func (photoStore possessionPhotoStore) Faces(ctx context.Context, photoID string) ([]possessions.PhotoFace, error) {
	store, scope, err := photoStore.store(ctx)
	if err != nil {
		return nil, err
	}
	found, err := store.Faces(ctx, scope, photos.FaceFilter{PhotoID: photoID})
	if err != nil {
		return nil, fmt.Errorf("photo-store, the faces on %s: %w", photoID, err)
	}
	faces := make([]possessions.PhotoFace, 0, len(found))
	for _, face := range found {
		faces = append(faces, possessions.PhotoFace{ID: face.ID,
			Box: possessions.Box{Left: face.Box.Left, Top: face.Box.Top, Right: face.Box.Right, Bottom: face.Box.Bottom}})
	}
	return faces, nil
}

// possessionPlaces is location-store's places as possession-store checks
// them.
type possessionPlaces struct {
	server *Server
}

var _ possessions.RecordFinder = possessionPlaces{}

// RecordExists is GET /places/{id} answering 200 rather than 404.
func (places possessionPlaces) RecordExists(ctx context.Context, id string) (bool, error) {
	stores := places.server.photosAndLocations.Load()
	if stores == nil {
		return false, errors.New("location-store is not open on this server")
	}
	scope, err := possessionReadScope(ctx, "location-store")
	if err != nil {
		return false, err
	}
	_, err = stores.locations.GetPlace(ctx, scope, id, false)
	if errors.Is(err, locations.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// possessionBudgetStore is budget-store as possession-store calls it: always
// in the books of the one owner the context names.
type possessionBudgetStore struct {
	server *Server
}

var (
	_ possessions.RecordFinder = possessionBudgetStore{}
	_ possessions.BudgetOrders = possessionBudgetStore{}
)

// inOwnersBooks runs work on the ledger of the owner ctx names.
func (budget possessionBudgetStore) inOwnersBooks(ctx context.Context, work func(*budget.Ledger) error) error {
	module := budget.server.budgetStore.Load()
	if module == nil {
		return errors.New("budget-store is not open on this server: EnableBudget was never called")
	}
	owner, err := possessionOwner(ctx, "budget-store")
	if err != nil {
		return err
	}
	ledger, err := module.store.OpenLedger(ctx, owner)
	if err != nil {
		return fmt.Errorf("budget-store, opening %s's books: %w", owner, err)
	}
	workErr := work(ledger)
	if err := ledger.Close(); err != nil && workErr == nil {
		return fmt.Errorf("budget-store, closing %s's books: %w", owner, err)
	}
	return workErr
}

// RecordExists is GET /transactions/{id} in the owner's books answering 200
// rather than 404.
func (budgetStore possessionBudgetStore) RecordExists(ctx context.Context, id string) (bool, error) {
	module := budgetStore.server.budgetStore.Load()
	if module == nil {
		return false, errors.New("budget-store is not open on this server: EnableBudget was never called")
	}
	owner, err := possessionOwner(ctx, "budget-store")
	if err != nil {
		return false, err
	}
	_, err = module.store.RecordInScope(ctx, ownership.OwnedBy(owner), budget.RecordKindTransaction, id)
	if errors.Is(err, budget.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// budgetStoreError marks a budget-store conflict as possession-store's, as
// the HTTP path marked a 409.
func budgetStoreError(call string, err error) error {
	if errors.Is(err, budget.ErrConflict) {
		return fmt.Errorf("%w: budget-store %s: %v", possessions.ErrConflict, call, err)
	}
	return fmt.Errorf("budget-store %s: %w", call, err)
}

func budgetOrderOf(order budget.MarketplaceOrder) possessions.BudgetOrder {
	read := possessions.BudgetOrder{ID: order.ID, Marketplace: order.Marketplace, OrderNumber: order.OrderNumber, Seller: order.Seller,
		OrderedAt: order.OrderedAt, TotalCents: order.TotalCents, Currency: order.Currency, TransactionID: order.TransactionID,
		MatchedBy: order.MatchedBy, MailAccountID: order.MailAccountID, MailMessageID: order.MailMessageID, ExtractedBy: order.ExtractedBy,
		CreatedBy: order.CreatedBy, Lines: make([]possessions.BudgetOrderLine, 0, len(order.Lines))}
	for _, line := range order.Lines {
		read.Lines = append(read.Lines, budgetOrderLineOf(line))
	}
	return read
}

func budgetOrderLineOf(line budget.MarketplaceOrderLine) possessions.BudgetOrderLine {
	return possessions.BudgetOrderLine{ID: line.ID, OrderID: line.OrderID, Title: line.Title, ItemNumber: line.ItemNumber,
		ListingURL: line.ListingURL, Quantity: line.Quantity, UnitPriceCents: line.UnitPriceCents, Disposition: line.Disposition,
		PossessionIDs: line.PossessionIDs}
}

func marketplaceOrderOf(order possessions.BudgetOrder) budget.MarketplaceOrder {
	written := budget.MarketplaceOrder{ID: order.ID, Marketplace: order.Marketplace, OrderNumber: order.OrderNumber, Seller: order.Seller,
		OrderedAt: order.OrderedAt, TotalCents: order.TotalCents, Currency: order.Currency, TransactionID: order.TransactionID,
		MatchedBy: order.MatchedBy, MailAccountID: order.MailAccountID, MailMessageID: order.MailMessageID, ExtractedBy: order.ExtractedBy,
		CreatedBy: order.CreatedBy}
	for _, line := range order.Lines {
		written.Lines = append(written.Lines, budget.MarketplaceOrderLine{ID: line.ID, OrderID: line.OrderID, Title: line.Title,
			ItemNumber: line.ItemNumber, ListingURL: line.ListingURL, Quantity: line.Quantity, UnitPriceCents: line.UnitPriceCents,
			Disposition: line.Disposition, PossessionIDs: line.PossessionIDs})
	}
	return written
}

// budgetOrderPage is how many orders one read takes, as the HTTP pages did.
const budgetOrderPage = 1000

func (budgetStore possessionBudgetStore) ListOrders(ctx context.Context) ([]possessions.BudgetOrder, error) {
	orders := []possessions.BudgetOrder{}
	err := budgetStore.inOwnersBooks(ctx, func(ledger *budget.Ledger) error {
		for offset := 0; ; offset += budgetOrderPage {
			page, err := ledger.ListMarketplaceOrders(budget.MarketplaceOrderFilter{Limit: budgetOrderPage, Offset: offset})
			if err != nil {
				return budgetStoreError("listing marketplace orders", err)
			}
			for _, order := range page {
				orders = append(orders, budgetOrderOf(order))
			}
			if len(page) < budgetOrderPage {
				return nil
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return orders, nil
}

func (budgetStore possessionBudgetStore) CreateOrder(ctx context.Context, order possessions.BudgetOrder) (possessions.BudgetOrder, error) {
	var created possessions.BudgetOrder
	err := budgetStore.inOwnersBooks(ctx, func(ledger *budget.Ledger) error {
		recorded, err := ledger.CreateMarketplaceOrder(marketplaceOrderOf(order))
		if err != nil {
			return budgetStoreError("recording a marketplace order", err)
		}
		created = budgetOrderOf(recorded)
		return nil
	})
	return created, err
}

func (budgetStore possessionBudgetStore) LinkPossession(ctx context.Context, lineID, possessionID, note string) error {
	return budgetStore.inOwnersBooks(ctx, func(ledger *budget.Ledger) error {
		if _, err := ledger.CreateLink(budget.Link{FromID: lineID, EntityType: "possession", EntityRef: possessionID, Note: note}); err != nil &&
			!errors.Is(err, budget.ErrConflict) {
			return budgetStoreError("linking "+lineID+" to "+possessionID, err)
		}
		if _, err := ledger.SetOrderLineDisposition(lineID, "possession"); err != nil {
			return budgetStoreError("marking "+lineID+" a possession", err)
		}
		return nil
	})
}

func (budgetStore possessionBudgetStore) OrderOfLine(ctx context.Context, lineID string) (possessions.BudgetOrder, possessions.BudgetOrderLine, error) {
	var order possessions.BudgetOrder
	var line possessions.BudgetOrderLine
	err := budgetStore.inOwnersBooks(ctx, func(ledger *budget.Ledger) error {
		readLine, err := ledger.GetMarketplaceOrderLine(lineID)
		if errors.Is(err, budget.ErrNotFound) {
			return fmt.Errorf("%w: budget-store has no order line %s", possessions.ErrInvalid, lineID)
		}
		if err != nil {
			return budgetStoreError("reading order line "+lineID, err)
		}
		readOrder, err := ledger.GetMarketplaceOrder(readLine.OrderID)
		if err != nil {
			return budgetStoreError("reading order "+readLine.OrderID, err)
		}
		order, line = budgetOrderOf(readOrder), budgetOrderLineOf(readLine)
		return nil
	})
	return order, line, err
}

// possessionMail is mailstack as possession-store reads it: as the one
// principal the context names, held to mailstack's own grants and cards.
type possessionMail struct {
	server *Server
}

var _ possessions.MailSource = possessionMail{}

// mailSearchPage is how many ids one search page asks for, as the HTTP
// search did.
const mailSearchPage = 100

func (mailbox possessionMail) caller(ctx context.Context) (*mail.Server, mail.Caller, error) {
	module := mailbox.server.mailstack.Load()
	if module == nil {
		return nil, mail.Caller{}, errMailstackOff
	}
	owner, err := possessionOwner(ctx, "mailstack")
	if err != nil {
		return nil, mail.Caller{}, err
	}
	return module, mail.Caller{PrincipalID: owner}, nil
}

func (mail possessionMail) Search(ctx context.Context, account, query, cursor string) ([]string, string, error) {
	module, caller, err := mail.caller(ctx)
	if err != nil {
		return nil, "", err
	}
	messages, next, err := module.ListMessagesAs(ctx, caller, account, "", query, cursor, mailSearchPage)
	if err != nil {
		return nil, "", fmt.Errorf("mailstack, searching %s: %w", account, err)
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids, next, nil
}

func (mail possessionMail) Message(ctx context.Context, account, id string) (possessions.MailMessage, error) {
	module, caller, err := mail.caller(ctx)
	if err != nil {
		return possessions.MailMessage{}, err
	}
	message, err := module.ReadMessageAs(ctx, caller, account, "", id)
	if err != nil {
		return possessions.MailMessage{}, fmt.Errorf("mailstack, reading %s: %w", id, err)
	}
	return possessions.MailMessageOf(message.Meta.ID, message.Meta.Subject, message.Meta.From.Email, message.Meta.Date,
		message.BodyText, message.BodyHTML), nil
}
