package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessions"
	"github.com/kayushkin/llm-bridge-server/internal/mail"
)

func asPossessionOwner(principalID string) context.Context {
	return ownership.ContextWithCaller(context.Background(), ownership.Caller{PrincipalID: principalID})
}

// possession-store's direct calls into photo-store, location-store and
// budget-store reach only the records of the owner the call names: another
// owner's record reads as missing, and a call naming nobody is refused.
func TestPossessionStoresCallsReachOnlyTheirOwnersRecords(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePossessionsOnTestDatabase(t, gated)
	owner, other := asPossessionOwner(firstTestPrincipalID), asPossessionOwner(secondTestPrincipalID)

	imported := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/photos/imports", map[string]any{"photos": []map[string]any{{
		"sha256": strings.Repeat("d", 64), "source_machine": "laptop", "source_path": "Pictures/Camera/Screenshot desk.png", "folder": "Pictures/Camera",
		"media_type": "image/jpeg", "taken_at": 1700000000, "date_source": "exif", "width": 40, "height": 30, "thumbnail_jpeg": testPhotoJPEG(t),
	}}})
	photoID, _ := decodeRecord(t, imported)["ids"].([]any)[0].(string)
	photos := possessionPhotoStore{server: gated.server}
	if exists, err := photos.RecordExists(owner, photoID); err != nil || !exists {
		t.Errorf("the owner's photo = %v, %v", exists, err)
	}
	if exists, err := photos.RecordExists(other, photoID); err != nil || exists {
		t.Errorf("another owner's photo = %v, %v; want not found", exists, err)
	}
	if _, err := photos.RecordExists(context.Background(), photoID); err == nil {
		t.Error("a photo check with no caller was answered")
	}
	source := possessions.PhotoStoreSource{PhotoStore: photos}
	if taken, err := source.TakenAt(owner, photoID); err != nil || taken != 1700000000 {
		t.Errorf("the owner's TakenAt = %d, %v", taken, err)
	}
	if why, err := source.WhyACopy(owner, photoID); err != nil || !strings.Contains(why, "screenshot") {
		t.Errorf("the owner's WhyACopy = %q, %v", why, err)
	}
	if thumbnail, err := source.Thumbnail(owner, photoID); err != nil || len(thumbnail) == 0 {
		t.Errorf("the owner's thumbnail = %d bytes, %v", len(thumbnail), err)
	}
	if _, err := source.Thumbnail(other, photoID); err == nil {
		t.Error("another owner read the thumbnail")
	}
	if _, err := source.TakenAt(other, photoID); err == nil {
		t.Error("another owner read the photo")
	}
	if _, err := source.WearerFace(owner, photoID, "person_000001"); !errors.Is(err, possessions.ErrInvalid) {
		t.Errorf("a wearer with no confirmed face = %v, want ErrInvalid", err)
	}

	place := gated.requestAsPrincipal(t, firstTestPrincipalID, "POST", "/locations/places",
		map[string]any{"name": "Example home", "latitude": 10.0, "longitude": 20.0, "kind": "home"})
	placeID, _ := decodeRecord(t, place)["id"].(string)
	places := possessionPlaces{server: gated.server}
	if exists, err := places.RecordExists(owner, placeID); err != nil || !exists {
		t.Errorf("the owner's place = %v, %v", exists, err)
	}
	if exists, err := places.RecordExists(other, placeID); err != nil || exists {
		t.Errorf("another owner's place = %v, %v; want not found", exists, err)
	}

	budget := possessionBudgetStore{server: gated.server}
	total := int64(2500)
	created, err := budget.CreateOrder(owner, possessions.BudgetOrder{Marketplace: "ebay", OrderNumber: "19-00000-00001", Seller: "seller_one",
		OrderedAt: 1700000000, TotalCents: &total, Currency: "USD", MailAccountID: "desk", MailMessageID: "m1", ExtractedBy: "model",
		CreatedBy: firstTestPrincipalID, Lines: []possessions.BudgetOrderLine{{Title: "desk lamp", Quantity: 1, UnitPriceCents: &total}}})
	if err != nil || created.ID == "" || len(created.Lines) != 1 {
		t.Fatalf("the owner's order = %+v, %v", created, err)
	}
	lineID := created.Lines[0].ID
	if _, err := budget.CreateOrder(owner, possessions.BudgetOrder{Marketplace: "ebay", OrderNumber: "19-00000-00001", OrderedAt: 1700000000,
		Currency: "USD", CreatedBy: firstTestPrincipalID, Lines: []possessions.BudgetOrderLine{{Title: "desk lamp"}}}); !errors.Is(err, possessions.ErrConflict) {
		t.Errorf("the same order again = %v, want ErrConflict", err)
	}
	if orders, err := budget.ListOrders(owner); err != nil || len(orders) != 1 {
		t.Errorf("the owner's orders = %d, %v", len(orders), err)
	}
	if orders, err := budget.ListOrders(other); err != nil || len(orders) != 0 {
		t.Errorf("another owner's orders = %d, %v; want none", len(orders), err)
	}
	if order, line, err := budget.OrderOfLine(owner, lineID); err != nil || order.ID != created.ID || line.ID != lineID {
		t.Errorf("the owner's line = %+v %+v %v", order, line, err)
	}
	if _, _, err := budget.OrderOfLine(other, lineID); !errors.Is(err, possessions.ErrInvalid) {
		t.Errorf("another owner's line = %v, want ErrInvalid", err)
	}
	if err := budget.LinkPossession(other, lineID, "possession_000001", "test"); err == nil {
		t.Error("another owner linked the owner's order line")
	}
	if err := budget.LinkPossession(owner, lineID, "possession_000001", "test"); err != nil {
		t.Errorf("the owner's link = %v", err)
	}
	if err := budget.LinkPossession(owner, lineID, "possession_000001", "test"); err != nil {
		t.Errorf("the owner's link made again = %v, want no error", err)
	}
	if _, line, err := budget.OrderOfLine(owner, lineID); err != nil || line.Disposition != "possession" || len(line.PossessionIDs) != 1 {
		t.Errorf("the linked line = %+v %v", line, err)
	}
	if _, err := budget.ListOrders(ownership.ContextWithCaller(context.Background(), ownership.Caller{SeesEveryOwner: true})); err == nil {
		t.Error("budget-store answered a call that names no owner")
	}
}

// possession-store reads mail as the one owner it acts for, held to
// mailstack's grants and cards as that owner's own request is.
func TestPossessionStoreReadsMailAsItsOwner(t *testing.T) {
	test := newMailstackTestServer(t)
	mailbox := possessionMail{server: test.server}
	member, administrator := asPossessionOwner(accessTestMemberID), asPossessionOwner(accessTestAdministratorID)

	if message, err := mailbox.Message(member, "desk", test.linkedMessageID); err != nil || message.Subject != "Printer jammed again" || message.From != "pat@demo.invalid" {
		t.Errorf("the member's message on its card = %+v, %v", message, err)
	}
	var refused *mail.MailRefusal
	if _, err := mailbox.Message(member, "desk", test.unlinkedMessageID); !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Errorf("a message on a card the member cannot see = %v, want 404", err)
	}
	if _, _, err := mailbox.Search(member, "desk", "", ""); !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Errorf("the member listing an account it holds no grant on = %v, want 403", err)
	}
	if ids, _, err := mailbox.Search(administrator, "desk", "", ""); err != nil || len(ids) != 2 {
		t.Errorf("the administrator's list = %v, %v", ids, err)
	}
	if _, err := mailbox.Message(context.Background(), "desk", test.linkedMessageID); err == nil {
		t.Error("mail was read for nobody")
	}
}
