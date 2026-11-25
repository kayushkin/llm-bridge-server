package server

// budget-store's passes, run in this process when their settings say so
// (internal/config/budget_store_settings.go). Each is off by default, since
// each writes into someone's books: bank sync brings in every linked bank's
// transactions through Plaid and pairs transfers, recurring bill detection
// suggests bills, interest matching links transactions to interests, and
// trip matching suggests the spending dated during each journey.
//
// A round runs every pass that is on once for each owner named in
// budget_store.passes.owner_principal_ids, in that owner's books only, so
// two owners' money is never matched together. Trip matching reads the
// owner's journeys from location-store in this process, narrowed to that
// owner. What a pass reports is logged as counts, never an amount or a
// description.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/budgetstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locationstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// budgetStorePassSettings is what the passes read at start.
type budgetStorePassSettings struct {
	owners                []string
	interval              time.Duration
	bankSync              bool
	recurringBills        bool
	interests             bool
	trips                 bool
	tripHomeMarkers       []string
	tripTimeZone          *time.Location
	tripTimeZoneName      string
	tripLookbackDays      int
	plaidIsOn             bool
	locationStoreIsServed bool
}

func (settings budgetStorePassSettings) anyOn() bool {
	return settings.bankSync || settings.recurringBills || settings.interests || settings.trips
}

// checkBudgetStorePassSettings refuses a combination that could not run.
// With every pass off nothing is checked.
func checkBudgetStorePassSettings(settings budgetStorePassSettings) error {
	if !settings.anyOn() {
		return nil
	}
	var problems []string
	for _, owner := range settings.owners {
		if err := ownership.CheckPrincipalID(config.SettingBudgetStorePassesOwnerPrincipalIDs, owner); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if settings.interval <= 0 {
		problems = append(problems, config.SettingBudgetStorePassesInterval+" must be longer than nothing")
	}
	if settings.bankSync && !settings.plaidIsOn {
		problems = append(problems, config.SettingBudgetStoreBankSyncEnabled+" is on, and Plaid is off: "+config.SettingBudgetStorePlaidEnvironment+" is empty")
	}
	if settings.trips {
		if len(settings.tripHomeMarkers) == 0 {
			problems = append(problems, config.SettingBudgetStoreTripMatchingHomeMarkers+" names no marker, and trip matching would suggest every charge made at home")
		}
		if settings.tripTimeZoneName == "" {
			problems = append(problems, config.SettingBudgetStoreTripMatchingTimeZone+" is empty")
		} else if settings.tripTimeZone == nil {
			problems = append(problems, config.SettingBudgetStoreTripMatchingTimeZone+" "+settings.tripTimeZoneName+" is not an IANA time zone")
		}
		if settings.tripLookbackDays <= 0 {
			problems = append(problems, config.SettingBudgetStoreTripMatchingLookbackDays+" must be at least 1")
		}
		if !settings.locationStoreIsServed {
			problems = append(problems, "trip matching reads location-store, which this server does not serve: EnablePhotosAndLocations must run first")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("budget-store passes: %s", strings.Join(problems, "; "))
	}
	return nil
}

// readBudgetStorePassSettings reads the settings and checks them.
func (s *Server) readBudgetStorePassSettings(module *budgetStoreModule) (budgetStorePassSettings, error) {
	settings := budgetStorePassSettings{
		owners:                s.settings.StringList(config.SettingBudgetStorePassesOwnerPrincipalIDs),
		interval:              s.settings.Duration(config.SettingBudgetStorePassesInterval),
		bankSync:              s.settings.Boolean(config.SettingBudgetStoreBankSyncEnabled),
		recurringBills:        s.settings.Boolean(config.SettingBudgetStoreRecurringBillDetectionEnabled),
		interests:             s.settings.Boolean(config.SettingBudgetStoreInterestMatchingEnabled),
		trips:                 s.settings.Boolean(config.SettingBudgetStoreTripMatchingEnabled),
		tripHomeMarkers:       s.settings.StringList(config.SettingBudgetStoreTripMatchingHomeMarkers),
		tripTimeZoneName:      s.settings.String(config.SettingBudgetStoreTripMatchingTimeZone),
		tripLookbackDays:      s.settings.Integer(config.SettingBudgetStoreTripMatchingLookbackDays),
		plaidIsOn:             module.store.PlaidIsOn(),
		locationStoreIsServed: s.photosAndLocations.Load() != nil,
	}
	if settings.tripTimeZoneName != "" {
		if location, err := time.LoadLocation(settings.tripTimeZoneName); err == nil {
			settings.tripTimeZone = location
		}
	}
	return settings, checkBudgetStorePassSettings(settings)
}

// StartBudgetStorePasses checks the pass settings and, when any pass is on,
// runs rounds until ctx ends. It refuses settings that could not run, so the
// server does not start with a pass that would only fail. main calls it
// after EnableBudgetStore.
func (s *Server) StartBudgetStorePasses(ctx context.Context) error {
	module := s.budgetStore.Load()
	if module == nil {
		return errors.New("budget-store passes: EnableBudgetStore must run first")
	}
	settings, err := s.readBudgetStorePassSettings(module)
	if err != nil {
		return err
	}
	if !settings.anyOn() {
		log.Printf("budget-store: no pass runs here (%s, %s, %s and %s are off)", config.SettingBudgetStoreBankSyncEnabled,
			config.SettingBudgetStoreRecurringBillDetectionEnabled, config.SettingBudgetStoreInterestMatchingEnabled, config.SettingBudgetStoreTripMatchingEnabled)
		return nil
	}
	if len(settings.owners) == 0 {
		log.Printf("budget-store: passes are on, and %s names no owner, so they run for nobody", config.SettingBudgetStorePassesOwnerPrincipalIDs)
		return nil
	}
	log.Printf("budget-store: passes (bank sync %t, recurring bills %t, interests %t, trips %t) run every %s for %d owners",
		settings.bankSync, settings.recurringBills, settings.interests, settings.trips, settings.interval, len(settings.owners))
	go func() {
		for {
			s.runBudgetStorePassRound(ctx, module.store, settings)
			select {
			case <-ctx.Done():
				return
			case <-time.After(settings.interval):
			}
		}
	}()
	return nil
}

// runBudgetStorePassRound runs every pass that is on for every owner. A pass
// that fails is logged and the round goes on: one owner's failure must not
// stop another's work, and the next round tries again.
func (s *Server) runBudgetStorePassRound(ctx context.Context, store *budgetstore.Store, settings budgetStorePassSettings) {
	for _, owner := range settings.owners {
		if ctx.Err() != nil {
			return
		}
		if err := s.runBudgetStorePassesForOwner(ctx, store, settings, owner); err != nil {
			log.Printf("[budget-store-passes] ERROR owner %s: %v", owner, err)
		}
	}
}

// runBudgetStorePassesForOwner runs, in owner's books, each pass that is on,
// in the order the standalone store's scheduler jobs ran them: the bank sync
// first, so the others see the new transactions.
func (s *Server) runBudgetStorePassesForOwner(ctx context.Context, store *budgetstore.Store, settings budgetStorePassSettings, owner string) error {
	books, err := store.OpenLedger(ctx, owner)
	if err != nil {
		return err
	}
	var errs []error
	defer func() {
		if err := books.Close(); err != nil {
			log.Printf("[budget-store-passes] ERROR owner %s: %v", owner, err)
		}
	}()
	prefix := "[budget-store-passes] " + owner
	if settings.bankSync {
		if err := runBudgetBankSync(ctx, books, prefix); err != nil {
			errs = append(errs, fmt.Errorf("bank sync: %w", err))
		}
	}
	if settings.recurringBills {
		result, err := books.DetectRecurringBills()
		if err != nil {
			errs = append(errs, fmt.Errorf("recurring bill detection: %w", err))
		} else {
			log.Printf("%s recurring bills: %d suggested, %d updated, %d ended, %d resumed", prefix, result.Suggested, result.Updated, result.Ended, result.Resumed)
		}
	}
	if settings.interests {
		added, err := books.MatchInterests()
		if err != nil {
			errs = append(errs, fmt.Errorf("interest matching: %w", err))
		} else {
			log.Printf("%s interests: %d transactions added", prefix, added)
		}
	}
	if settings.trips {
		suggested, journeys, err := s.runBudgetTripMatching(ctx, books, settings, owner)
		if err != nil {
			errs = append(errs, fmt.Errorf("trip matching: %w", err))
		} else {
			log.Printf("%s trips: %d suggested across %d journeys", prefix, suggested, journeys)
		}
	}
	return errors.Join(errs...)
}

// runBudgetBankSync syncs every linked bank, then pairs transfers and
// applies payee rules, as the standalone store's scheduler job did. A bank
// that failed is kept as its item's last_error, which GET
// /budget-store/plaid/items shows; the log names only how many failed.
func runBudgetBankSync(ctx context.Context, books *budgetstore.Ledger, prefix string) error {
	synced, err := books.SyncPlaid(ctx)
	if err != nil {
		return err
	}
	log.Printf("%s bank sync: %d added, %d updated, %d voided, %d before sync start, %d in unmatched accounts, %d banks failed",
		prefix, synced.Added, synced.Updated, synced.Voided, synced.BeforeSyncStart, synced.UnmatchedAccount, len(synced.Errors))
	transfers, err := books.MatchTransfers()
	if err != nil {
		return fmt.Errorf("matching transfers: %w", err)
	}
	log.Printf("%s transfers: %d paired, %d moved without a partner", prefix, transfers.Paired, transfers.AssignedWithoutPartner)
	payees, err := books.ApplyPayeeRules()
	if err != nil {
		return fmt.Errorf("applying payee rules: %w", err)
	}
	log.Printf("%s payee rules: %d payees set, %d categories changed", prefix, payees.PayeesSet, payees.CategoriesChanged)
	if len(synced.Errors) > 0 {
		return fmt.Errorf("%d linked banks did not sync; GET /budget-store/plaid/items shows each one's last_error", len(synced.Errors))
	}
	return nil
}

// budgetTripJourneyPage is how many journeys one read of location-store
// takes: its most.
const budgetTripJourneyPage = 1000

// runBudgetTripMatching reads owner's journeys that ended in the lookback
// from location-store, narrowed to owner, and suggests the spending dated
// during each. Answers how many links it suggested and how many journeys it
// read.
func (s *Server) runBudgetTripMatching(ctx context.Context, books *budgetstore.Ledger, settings budgetStorePassSettings, owner string) (int, int, error) {
	locations := s.photosAndLocations.Load()
	if locations == nil {
		return 0, 0, errors.New("location-store is not served here")
	}
	since := time.Now().In(settings.tripTimeZone).AddDate(0, 0, -settings.tripLookbackDays)
	sinceDay := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, settings.tripTimeZone)
	var journeys []budgetstore.TripJourney
	for offset := 0; ; offset += budgetTripJourneyPage {
		page, err := locations.locations.ListTrips(ctx, ownership.OwnedBy(owner), locationstore.TripFilter{
			TimeFilter: locationstore.TimeFilter{From: sinceDay.Unix(), Limit: budgetTripJourneyPage, Offset: offset},
			Kind:       locationstore.TripKindJourney,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("reading journeys from location-store: %w", err)
		}
		for _, trip := range page {
			journeys = append(journeys, budgetstore.TripJourney{ID: trip.ID, StartedAt: trip.StartedAt, EndedAt: trip.EndedAt})
		}
		if len(page) < budgetTripJourneyPage {
			break
		}
	}
	suggested, err := books.SuggestForJourneys(journeys, settings.tripTimeZone, settings.tripHomeMarkers, sinceDay.Format("2006-01-02"))
	return suggested, len(journeys), err
}
