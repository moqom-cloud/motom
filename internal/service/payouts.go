// Copyright 2026 Elliott Wave LLC. All rights reserved.

package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/stripe"
	"github.com/moqom-cloud/motom/ledger"
)

// Payouts use Stripe Connect Express with separate charges and transfers:
// buyers pay MOTOM's platform account, creator earnings are a liability in the
// ledger, and a payout is a Stripe transfer to the creator's connected
// account, from which Stripe pays their bank. Stripe holds the funds and does
// the creator's KYC; MOTOM never holds a bank account number or an ID document.

// ErrNotPayable is a creator who cannot be paid yet.
var ErrNotPayable = errors.New("creator cannot be paid out")

// MaxPayoutAttempts is how many runs retry a transfer whose outcome is
// unknown before it is put in front of a person.
const MaxPayoutAttempts = 5

// IdempotencyWindow is how long after a payout is created its transfer may
// be retried automatically: inside Stripe's 24-hour idempotency-key memory,
// with an hour to spare.
const IdempotencyWindow = 23 * time.Hour

func (s *Service) updatePayee(ctx context.Context, tenant, creator string, change func(*store.Payee) bool) (store.Payee, error) {
	for attempt := 0; attempt < 8; attempt++ {
		p, err := s.store.GetPayee(ctx, tenant, creator)
		if errors.Is(err, store.ErrNotFound) {
			now := s.now()
			p = store.Payee{Tenant: tenant, Creator: creator, KYC: store.KYCNone, CreatedAt: now}
		} else if err != nil {
			return store.Payee{}, err
		}
		if !change(&p) {
			return p, nil
		}
		p.UpdatedAt = s.now()
		err = s.store.SavePayee(ctx, p)
		if errors.Is(err, store.ErrVersionConflict) {
			continue
		}
		if err != nil {
			return store.Payee{}, err
		}
		return s.store.GetPayee(ctx, tenant, creator)
	}
	return store.Payee{}, ErrConflict
}

// Onboard creates the creator's Express account if they have none and
// returns a Stripe-hosted onboarding link.
func (s *Service) Onboard(ctx context.Context, actor, tenant, creator, country, refreshURL, returnTo string) (string, store.Payee, error) {
	if s.stripe == nil {
		return "", store.Payee{}, ErrPaymentsUnavailable
	}
	if err := validID("creator", creator); err != nil {
		return "", store.Payee{}, err
	}
	if country != "" && !countryCode(country) {
		return "", store.Payee{}, fmt.Errorf("%w: country is a two-letter ISO code", ErrInvalid)
	}
	if err := returnURL(refreshURL); err != nil {
		return "", store.Payee{}, err
	}
	if err := returnURL(returnTo); err != nil {
		return "", store.Payee{}, err
	}
	if _, err := s.store.GetTenant(ctx, tenant); err != nil {
		return "", store.Payee{}, err
	}
	payee, err := s.store.GetPayee(ctx, tenant, creator)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", store.Payee{}, err
	}
	if payee.StripeAccount == "" {
		account, err := s.stripe.CreateExpressAccount(ctx, country,
			map[string]string{"motom_tenant": tenant, "motom_creator": creator}, "motom:acct:"+tenant+":"+creator)
		if err != nil {
			return "", store.Payee{}, err
		}
		payee, err = s.updatePayee(ctx, tenant, creator, func(p *store.Payee) bool {
			if p.StripeAccount != "" {
				return false
			}
			p.StripeAccount, p.KYC = account.ID, store.KYCPending
			return true
		})
		if err != nil {
			return "", store.Payee{}, err
		}
		s.audit(ctx, tenant, actor, "payee.onboard", "payee:"+creator, payee.StripeAccount)
	}
	link, err := s.stripe.CreateAccountLink(ctx, payee.StripeAccount, refreshURL, returnTo)
	return link, payee, err
}

func countryCode(c string) bool {
	return len(c) == 2 && c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z'
}

func (s *Service) accountUpdated(ctx context.Context, account stripe.Account) error {
	payee, err := s.store.FindPayeeByStripeAccount(ctx, account.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	kyc := store.KYCPending
	switch {
	case account.Requirements.DisabledReason != "":
		kyc = store.KYCRestricted
	case account.PayoutsEnabled && account.DetailsSubmitted:
		kyc = store.KYCVerified
	}
	updated, err := s.updatePayee(ctx, payee.Tenant, payee.Creator, func(p *store.Payee) bool {
		if p.KYC == kyc && p.PayoutsEnabled == account.PayoutsEnabled && p.DisabledReason == account.Requirements.DisabledReason {
			return false
		}
		p.KYC, p.PayoutsEnabled, p.DisabledReason = kyc, account.PayoutsEnabled, account.Requirements.DisabledReason
		return true
	})
	if err != nil {
		return err
	}
	if updated.Version != payee.Version {
		s.audit(ctx, payee.Tenant, "stripe", "payee.kyc", "payee:"+payee.Creator, string(kyc))
	}
	return nil
}

// Earnings is a creator's money in one currency.
type Earnings struct {
	Currency ledger.Currency
	// Balance is everything earned and not yet paid out.
	Balance ledger.Amount
	// Held is the part credited within the hold period.
	Held ledger.Amount
	// Available is what the next payout run may pay.
	Available ledger.Amount
	// Pending is committed to a payout Stripe has not confirmed.
	Pending ledger.Amount
}

// PayeeState is a creator's payout standing.
type PayeeState struct {
	Payee       store.Payee
	Earnings    []Earnings
	OpenReviews int
}

func (s *Service) earnings(ctx context.Context, t store.Tenant, creator string, currency ledger.Currency) (Earnings, int64, error) {
	e := Earnings{Currency: currency}
	id := ledger.AccountFor(t.ID, ledger.Earnings, creator, currency)
	balance, version, err := s.normalBalance(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return e, 0, nil
	}
	if err != nil {
		return Earnings{}, 0, err
	}
	var held ledger.Amount
	if t.Config.HoldDays > 0 {
		if held, err = s.store.CreditsSince(ctx, id, s.now().AddDate(0, 0, -t.Config.HoldDays)); err != nil {
			return Earnings{}, 0, err
		}
	}
	e.Balance, e.Held = balance, min(held, max(balance, 0))
	e.Available = max(balance-e.Held, 0)
	if pending, _, err := s.normalBalance(ctx, ledger.AccountFor(t.ID, ledger.PendingPayout, creator, currency)); err == nil {
		e.Pending = pending
	} else if !errors.Is(err, store.ErrNotFound) {
		return Earnings{}, 0, err
	}
	return e, version, nil
}

// Payee returns a creator's payout standing.
func (s *Service) Payee(ctx context.Context, tenant, creator string) (PayeeState, error) {
	if err := validID("creator", creator); err != nil {
		return PayeeState{}, err
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return PayeeState{}, err
	}
	payee, err := s.store.GetPayee(ctx, tenant, creator)
	if errors.Is(err, store.ErrNotFound) {
		payee = store.Payee{Tenant: tenant, Creator: creator, KYC: store.KYCNone}
	} else if err != nil {
		return PayeeState{}, err
	}
	state := PayeeState{Payee: payee}
	for _, c := range t.Currencies {
		e, _, err := s.earnings(ctx, t, creator, c)
		if err != nil {
			return PayeeState{}, err
		}
		state.Earnings = append(state.Earnings, e)
	}
	open, err := s.store.ListReviews(ctx, tenant, store.ReviewOpen, "payee:"+creator)
	if err != nil {
		return PayeeState{}, err
	}
	state.OpenReviews = len(open)
	return state, nil
}

// SetPayeeFrozen freezes or unfreezes a creator's payouts.
func (s *Service) SetPayeeFrozen(ctx context.Context, actor, tenant, creator string, frozen bool) (store.Payee, error) {
	p, err := s.updatePayee(ctx, tenant, creator, func(p *store.Payee) bool {
		if p.Frozen == frozen {
			return false
		}
		p.Frozen = frozen
		return true
	})
	if err == nil {
		s.audit(ctx, tenant, actor, map[bool]string{true: "payee.freeze", false: "payee.unfreeze"}[frozen], "payee:"+creator, "")
	}
	return p, err
}

// RunReport is what a payout run did.
type RunReport struct {
	Tenant  string         `json:"tenant"`
	Run     string         `json:"run"`
	Paid    []string       `json:"paid"`
	Failed  []string       `json:"failed"`
	Pending []string       `json:"pending"`
	Skipped map[string]int `json:"skipped"`
}

// Payouts lists a tenant's payouts.
func (s *Service) Payouts(ctx context.Context, q store.PayoutQuery) ([]store.Payout, error) {
	return s.store.ListPayouts(ctx, q)
}

// RunPayouts pays every payable creator of a tenant what is available, for
// the named run (the UTC date when empty). Re-running a run is safe: each
// payout's ID, ledger key and Stripe idempotency key derive from the run, the
// creator and the currency, so it resumes rather than repeats.
func (s *Service) RunPayouts(ctx context.Context, actor, tenant, run string) (RunReport, error) {
	if s.stripe == nil {
		return RunReport{}, ErrPaymentsUnavailable
	}
	if run == "" {
		run = s.now().Format(time.DateOnly)
	}
	if err := validID("run", run); err != nil {
		return RunReport{}, err
	}
	t, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return RunReport{}, err
	}
	report := RunReport{Tenant: tenant, Run: run, Skipped: map[string]int{}}
	pending, err := s.store.ListPayouts(ctx, store.PayoutQuery{Tenant: tenant, Status: store.PayoutPending})
	if err != nil {
		return RunReport{}, err
	}
	for _, p := range pending {
		s.record(&report, s.settle(ctx, p))
	}
	payees, err := s.store.ListPayees(ctx, tenant)
	if err != nil {
		return RunReport{}, err
	}
	for _, payee := range payees {
		if !payee.Payable() {
			report.Skipped[skipReason(payee)]++
			continue
		}
		open, err := s.store.ListReviews(ctx, tenant, store.ReviewOpen, "payee:"+payee.Creator)
		if err != nil {
			return report, err
		}
		if len(open) > 0 {
			report.Skipped["under_review"]++
			continue
		}
		for _, currency := range t.Currencies {
			p, reason, err := s.startPayout(ctx, actor, t, payee, currency, run)
			if err != nil {
				return report, err
			}
			if reason != "" {
				report.Skipped[reason]++
				continue
			}
			s.record(&report, s.settle(ctx, p))
		}
	}
	s.audit(ctx, tenant, actor, "payouts.run", "run:"+run,
		fmt.Sprintf(`{"paid":%d,"failed":%d,"pending":%d}`, len(report.Paid), len(report.Failed), len(report.Pending)))
	return report, nil
}

// RunAllPayouts runs payouts for every tenant, for the scheduled job.
func (s *Service) RunAllPayouts(ctx context.Context, run string) ([]RunReport, error) {
	tenants, err := s.store.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	var reports []RunReport
	var errs []error
	for _, t := range tenants {
		report, err := s.RunPayouts(ctx, "scheduler", t.ID, run)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t.ID, err))
		}
		reports = append(reports, report)
	}
	return reports, errors.Join(errs...)
}

func skipReason(p store.Payee) string {
	switch {
	case p.Frozen:
		return "frozen"
	case p.StripeAccount == "":
		return "not_onboarded"
	default:
		return "kyc_" + string(p.KYC)
	}
}

func (s *Service) record(report *RunReport, p store.Payout) {
	switch p.Status {
	case store.PayoutPaid:
		report.Paid = append(report.Paid, p.ID)
	case store.PayoutFailed:
		report.Failed = append(report.Failed, p.ID)
	case store.PayoutPending:
		report.Pending = append(report.Pending, p.ID)
	}
}

// startPayout commits a payout in the ledger: earnings move to pending
// before any money moves at Stripe. It returns a skip reason instead when
// there is nothing to pay or a person must look first.
func (s *Service) startPayout(ctx context.Context, actor string, t store.Tenant, payee store.Payee, currency ledger.Currency, run string) (store.Payout, string, error) {
	id := derivedID("po", t.ID, payee.Creator, string(currency), run)
	// A payout this run already made was settled (or retried) with the
	// pending ones at the start of the run.
	if _, err := s.store.GetPayout(ctx, t.ID, id); err == nil {
		return store.Payout{}, "already_run", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Payout{}, "", err
	}
	if e, ok, err := s.replay(ctx, t.ID, "payout:"+id); err != nil {
		return store.Payout{}, "", err
	} else if ok {
		// The entry committed and the record did not: rebuild the record.
		return s.createPayout(ctx, t.ID, id, payee.Creator, currency, e.Postings[0].Amount, run, e)
	}
	earnings, version, err := s.earnings(ctx, t, payee.Creator, currency)
	if err != nil {
		return store.Payout{}, "", err
	}
	if earnings.Available < t.Config.MinPayout {
		return store.Payout{}, "below_minimum", nil
	}
	if !payee.PaidOnce && earnings.Available >= t.Config.ReviewFirstPayoutAt {
		approved, err := s.approved(ctx, t.ID, "payee:"+payee.Creator, "first_payout")
		if err != nil {
			return store.Payout{}, "", err
		}
		if !approved {
			if _, err := s.flag(ctx, t.ID, "payee:"+payee.Creator, "first_payout",
				fmt.Sprintf("first payout of %d %s", earnings.Available, currency), run); err != nil {
				return store.Payout{}, "", err
			}
			return store.Payout{}, "under_review", nil
		}
	}
	ids, err := s.accounts(ctx, t.ID, currency, accountSpec{ledger.Earnings, payee.Creator}, accountSpec{ledger.PendingPayout, payee.Creator})
	if err != nil {
		return store.Payout{}, "", err
	}
	amount := earnings.Available
	e, _, err := s.post(ctx, ledger.Entry{Tenant: t.ID, Kind: ledger.KindPayoutInitiated, IdempotencyKey: "payout:" + id,
		Postings:  []ledger.Posting{{Account: ids[0], Amount: amount}, {Account: ids[1], Amount: -amount}},
		Reference: id, Actor: actor, Expect: map[ledger.AccountID]int64{ids[0]: version},
		Metadata: map[string]string{"payout": id, "creator": payee.Creator, "run": run}})
	if errors.Is(err, store.ErrVersionConflict) || errors.Is(err, store.ErrInsufficientFunds) {
		// Earnings moved while we computed what was available; the next run
		// will see the settled figure.
		return store.Payout{}, "busy", nil
	}
	if err != nil {
		return store.Payout{}, "", err
	}
	return s.createPayout(ctx, t.ID, id, payee.Creator, currency, amount, run, e)
}

func (s *Service) createPayout(ctx context.Context, tenant, id, creator string, currency ledger.Currency, amount ledger.Amount, run string, e ledger.Entry) (store.Payout, string, error) {
	if amount < 0 {
		amount = -amount
	}
	now := s.now()
	p := store.Payout{ID: id, Tenant: tenant, Creator: creator, Currency: currency, Amount: amount,
		Status: store.PayoutPending, Run: run, EntryID: e.ID, CreatedAt: e.CreatedAt, UpdatedAt: now}
	if err := s.store.CreatePayout(ctx, p); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return store.Payout{}, "", err
	}
	p, err := s.store.GetPayout(ctx, tenant, id)
	return p, "", err
}

func (s *Service) updatePayout(ctx context.Context, tenant, id string, change func(*store.Payout) bool) (store.Payout, error) {
	for attempt := 0; attempt < 8; attempt++ {
		p, err := s.store.GetPayout(ctx, tenant, id)
		if err != nil {
			return store.Payout{}, err
		}
		if !change(&p) {
			return p, nil
		}
		p.UpdatedAt = s.now()
		err = s.store.SavePayout(ctx, p)
		if errors.Is(err, store.ErrVersionConflict) {
			continue
		}
		if err != nil {
			return store.Payout{}, err
		}
		return s.store.GetPayout(ctx, tenant, id)
	}
	return store.Payout{}, ErrConflict
}

// settle asks Stripe for the transfer behind a pending payout and records
// the answer. It returns the payout as it now stands; errors become state
// (failed, or still pending) rather than aborting the run.
func (s *Service) settle(ctx context.Context, p store.Payout) store.Payout {
	fail := func(why string) store.Payout {
		entry, err := s.store.GetEntry(ctx, p.Tenant, p.EntryID)
		if err == nil {
			err = s.unwind(ctx, entry, "payout failed: "+why)
		}
		if err != nil {
			s.log.ErrorContext(ctx, "payout unwind failed", "event", "motom_payout_unwind_failed", "payout", p.ID, "error", err)
			return p
		}
		updated, err := s.updatePayout(ctx, p.Tenant, p.ID, func(p *store.Payout) bool {
			if p.Status != store.PayoutPending {
				return false
			}
			p.Status, p.Failure = store.PayoutFailed, why
			return true
		})
		if err != nil {
			return p
		}
		s.audit(ctx, p.Tenant, "motom", "payout.failed", "payee:"+p.Creator, why)
		return updated
	}
	payee, err := s.store.GetPayee(ctx, p.Tenant, p.Creator)
	if err != nil {
		return p
	}
	if p.Attempts > 0 {
		// An earlier attempt may have reached Stripe. Only the same
		// idempotency key may ask again, and Stripe forgets keys after 24
		// hours; past that a person reconciles against the dashboard
		// (transfer_group motom:<run>, metadata motom_payout).
		if s.now().Sub(p.CreatedAt) > IdempotencyWindow {
			_, _ = s.flag(ctx, p.Tenant, "payee:"+p.Creator, "payout_stuck",
				fmt.Sprintf("payout %s unconfirmed past Stripe's idempotency window; reconcile by hand", p.ID), p.ID)
			return p
		}
	} else if !payee.Payable() {
		return fail("payee no longer payable: " + skipReason(payee))
	}
	transfer, err := s.stripe.CreateTransfer(ctx, int64(p.Amount), string(p.Currency), payee.StripeAccount, "motom:"+p.Run,
		map[string]string{"motom_tenant": p.Tenant, "motom_payout": p.ID, "motom_creator": p.Creator}, "motom:"+p.ID)
	if err != nil {
		if stripe.Definitive(err) {
			return fail(err.Error())
		}
		updated, uerr := s.updatePayout(ctx, p.Tenant, p.ID, func(p *store.Payout) bool {
			p.Attempts++
			p.Failure = err.Error()
			return true
		})
		if uerr == nil && updated.Attempts >= MaxPayoutAttempts {
			_, _ = s.flag(ctx, p.Tenant, "payee:"+p.Creator, "payout_stuck",
				fmt.Sprintf("payout %s unconfirmed after %d attempts: %v", p.ID, updated.Attempts, err), p.ID)
		}
		s.log.WarnContext(ctx, "payout transfer outcome unknown", "event", "motom_payout_retry", "payout", p.ID, "error", err)
		if uerr != nil {
			return p
		}
		return updated
	}
	ids, err := s.accounts(ctx, p.Tenant, p.Currency, accountSpec{ledger.PendingPayout, p.Creator}, accountSpec{ledger.StripeClearing, ""})
	if err != nil {
		return p
	}
	settled, _, err := s.post(ctx, ledger.Entry{Tenant: p.Tenant, Kind: ledger.KindPayoutTransferred, IdempotencyKey: "payout-settle:" + p.ID,
		Postings:  []ledger.Posting{{Account: ids[0], Amount: p.Amount}, {Account: ids[1], Amount: -p.Amount}},
		Reference: transfer.ID, Actor: "stripe", Metadata: map[string]string{"payout": p.ID, "creator": p.Creator}})
	if err != nil {
		s.log.ErrorContext(ctx, "payout settlement entry failed", "event", "motom_payout_settle_failed", "payout", p.ID, "error", err)
		return p
	}
	updated, err := s.updatePayout(ctx, p.Tenant, p.ID, func(p *store.Payout) bool {
		if p.Status == store.PayoutPaid {
			return false
		}
		p.Status, p.StripeTransfer, p.SettleEntryID, p.Failure = store.PayoutPaid, transfer.ID, settled.ID, ""
		return true
	})
	if err != nil {
		return p
	}
	_, _ = s.updatePayee(ctx, p.Tenant, p.Creator, func(payee *store.Payee) bool {
		if payee.PaidOnce {
			return false
		}
		payee.PaidOnce = true
		return true
	})
	return updated
}

func (s *Service) transferReversed(ctx context.Context, transfer stripe.Transfer) error {
	p, err := s.store.FindPayoutByTransfer(ctx, transfer.ID)
	if errors.Is(err, store.ErrNotFound) && transfer.Metadata["motom_payout"] != "" {
		p, err = s.store.GetPayout(ctx, transfer.Metadata["motom_tenant"], transfer.Metadata["motom_payout"])
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	amount := min(ledger.Amount(transfer.AmountReversed), p.Amount)
	if amount <= 0 {
		return nil
	}
	ids, err := s.accounts(ctx, p.Tenant, p.Currency, accountSpec{ledger.StripeClearing, ""}, accountSpec{ledger.Earnings, p.Creator})
	if err != nil {
		return err
	}
	// The money is back in MOTOM's Stripe balance and owed to the creator
	// again; it re-enters the hold like any new earning.
	_, _, err = s.post(ctx, ledger.Entry{Tenant: p.Tenant, Kind: ledger.KindPayoutReturned,
		IdempotencyKey: "stripe:transfer-reversed:" + transfer.ID + ":" + strconv.FormatInt(int64(amount), 10),
		Postings:       []ledger.Posting{{Account: ids[0], Amount: amount}, {Account: ids[1], Amount: -amount}},
		Reference:      transfer.ID, Actor: "stripe", Metadata: map[string]string{"payout": p.ID, "creator": p.Creator}})
	if err != nil {
		return err
	}
	if _, err := s.updatePayout(ctx, p.Tenant, p.ID, func(p *store.Payout) bool {
		if p.Status == store.PayoutReturned {
			return false
		}
		p.Status = store.PayoutReturned
		return true
	}); err != nil {
		return err
	}
	_, err = s.flag(ctx, p.Tenant, "payee:"+p.Creator, "transfer_reversed", fmt.Sprintf("transfer %s reversed (%d)", transfer.ID, amount), transfer.ID)
	return err
}
