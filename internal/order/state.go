// Package order defines transaction-independent order rules. The caller must
// lock and re-read persisted state, deduplicate external transactions, and commit
// the decision together with its inventory, payment, and voucher records.
package order

import (
	"errors"
	"fmt"
)

type State string

const (
	PendingPayment State = "PENDING_PAYMENT"
	Paid           State = "PAID"
	Closed         State = "CLOSED"
	Refunding      State = "REFUNDING"
	Refunded       State = "REFUNDED"
	Redeemed       State = "REDEEMED"
)

var (
	ErrInvalidState      = errors.New("invalid order state")
	ErrInvalidTransition = errors.New("invalid order transition")
	ErrInvalidRefund     = errors.New("invalid refund decision")
)

func (s State) Valid() bool {
	switch s {
	case PendingPayment, Paid, Closed, Refunding, Refunded, Redeemed:
		return true
	default:
		return false
	}
}

// ValidateTransition validates a state change, not an idempotent replay. A caller
// handles an already committed operation by returning its recorded result.
// REFUNDING -> PAID additionally requires a confirmed permanent refund failure;
// use DecideRefund to validate that condition. Permissions, deadlines, amounts,
// and inventory remain transaction-level checks.
func ValidateTransition(from, to State) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidState, from, to)
	}
	allowed := false
	switch from {
	case PendingPayment:
		allowed = to == Paid || to == Closed
	case Paid:
		allowed = to == Refunding || to == Redeemed
	case Refunding:
		allowed = to == Refunded || to == Paid
	}
	if !allowed {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

type RefundPurpose string

const (
	NormalRefund       RefundPurpose = "NORMAL"
	LatePaymentRefund  RefundPurpose = "LATE_PAYMENT"
	ExtraPaymentRefund RefundPurpose = "EXTRA_PAYMENT"
)

type PaymentOutcome string

const (
	PaymentApplied   PaymentOutcome = "APPLIED"
	PaymentDuplicate PaymentOutcome = "DUPLICATE"
	PaymentLate      PaymentOutcome = "LATE_PAYMENT"
	PaymentExtra     PaymentOutcome = "EXTRA_PAYMENT"
)

type PaymentSettlement struct {
	Outcome       PaymentOutcome
	NextState     State
	IssueVoucher  bool
	RefundPurpose RefundPurpose // Empty when no compensation refund is needed.
}

// DecidePayment classifies a verified successful payment. alreadyRecorded means
// this exact external transaction's business decision was committed, not merely
// that a payment attempt exists. Different successful transaction IDs must be
// classified separately. This function never authorizes an inventory release.
func DecidePayment(current State, alreadyRecorded bool) (PaymentSettlement, error) {
	if !current.Valid() {
		return PaymentSettlement{}, fmt.Errorf("%w: %q", ErrInvalidState, current)
	}
	decision := PaymentSettlement{NextState: current}
	switch {
	case alreadyRecorded:
		decision.Outcome = PaymentDuplicate
	case current == PendingPayment:
		decision.Outcome = PaymentApplied
		decision.NextState = Paid
		decision.IssueVoucher = true
	case current == Closed:
		decision.Outcome = PaymentLate
		decision.RefundPurpose = LatePaymentRefund
	default:
		decision.Outcome = PaymentExtra
		decision.RefundPurpose = ExtraPaymentRefund
	}
	return decision, nil
}

type RefundResult string

const (
	RefundUnknown     RefundResult = "UNKNOWN"
	RefundSucceeded   RefundResult = "SUCCEEDED"
	RefundFailedFinal RefundResult = "FAILED_FINAL"
)

type RefundSettlement struct {
	NextState        State
	ReleaseInventory bool
}

// DecideRefund handles a verified, not-yet-applied refund result. The caller must
// check that this is the order's current refund operation and deduplicate its
// external result. ReleaseInventory is a transaction intent, not proof that a
// RELEASE movement may be inserted again. Its uniqueness is enforced in storage.
// Restoring PAID never extends the voucher's independent expiration time.
func DecideRefund(current State, purpose RefundPurpose, result RefundResult) (RefundSettlement, error) {
	if !current.Valid() {
		return RefundSettlement{}, fmt.Errorf("%w: %q", ErrInvalidState, current)
	}
	if result != RefundUnknown && result != RefundSucceeded && result != RefundFailedFinal {
		return RefundSettlement{}, fmt.Errorf("%w: result %q", ErrInvalidRefund, result)
	}
	decision := RefundSettlement{NextState: current}
	switch purpose {
	case NormalRefund:
		if current != Refunding {
			return RefundSettlement{}, fmt.Errorf("%w: normal refund from %s", ErrInvalidRefund, current)
		}
		switch result {
		case RefundSucceeded:
			decision.NextState = Refunded
			decision.ReleaseInventory = true
		case RefundFailedFinal:
			decision.NextState = Paid
		}
	case LatePaymentRefund:
		if current != Closed {
			return RefundSettlement{}, fmt.Errorf("%w: late payment refund from %s", ErrInvalidRefund, current)
		}
	case ExtraPaymentRefund:
		if current != Paid && current != Refunding && current != Refunded && current != Redeemed {
			return RefundSettlement{}, fmt.Errorf("%w: extra payment refund from %s", ErrInvalidRefund, current)
		}
	default:
		return RefundSettlement{}, fmt.Errorf("%w: purpose %q", ErrInvalidRefund, purpose)
	}
	return decision, nil
}
