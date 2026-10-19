// Package payment provides an in-memory, deterministic payment-provider
// simulator for local business-flow tests. It does not store credentials or
// communicate with a real payment provider.
package payment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
)

var (
	ErrInvalidAmount       = errors.New("payment amount must be positive")
	ErrEmptyCurrency       = errors.New("payment currency is required")
	ErrEmptyAttemptID      = errors.New("payment attempt ID is required")
	ErrEmptyRefundID       = errors.New("refund request ID is required")
	ErrEmptyProviderTxnID  = errors.New("provider transaction ID is required")
	ErrIdempotencyConflict = errors.New("idempotency key was reused with different parameters")
	ErrAttemptNotFound     = errors.New("payment attempt was not found")
	ErrRefundNotFound      = errors.New("refund request was not found")
	ErrInvalidStatus       = errors.New("invalid payment status")
	ErrNotUnknown          = errors.New("only an unknown result can be resolved")
)

// Status is the result reported by the simulated provider.
type Status string

const (
	StatusSuccess     Status = "SUCCESS"
	StatusUnknown     Status = "UNKNOWN"
	StatusFailedFinal Status = "FAILED_FINAL"
)

// ChargeRequest represents one stable payment-attempt request. Amount is in
// the smallest currency unit. InitialStatus permits SUCCESS or UNKNOWN.
type ChargeRequest struct {
	AttemptID     string
	OrderID       string
	Amount        int64
	Currency      string
	InitialStatus Status
}

// ChargeResult is safe to persist for local tests; it contains no credential.
type ChargeResult struct {
	ProviderTransactionID string
	Status                Status
}

// RefundRequest represents one stable refund request. InitialStatus permits
// SUCCESS, UNKNOWN, or FAILED_FINAL.
type RefundRequest struct {
	RefundRequestID       string
	ProviderTransactionID string
	Amount                int64
	Currency              string
	InitialStatus         Status
}

// RefundResult is the local provider result for one refund request.
type RefundResult struct {
	ProviderRefundID string
	Status           Status
}

type chargeRecord struct {
	request ChargeRequest
	result  ChargeResult
}

type refundRecord struct {
	request RefundRequest
	result  RefundResult
}

// Simulator is safe for concurrent callers. Its state lives only in memory,
// so a process restart intentionally loses pending provider observations.
type Simulator struct {
	mu      sync.Mutex
	charges map[string]chargeRecord
	refunds map[string]refundRecord
}

func NewSimulator() *Simulator {
	return &Simulator{
		charges: make(map[string]chargeRecord),
		refunds: make(map[string]refundRecord),
	}
}

// Charge creates at most one provider transaction for an AttemptID. Repeating
// the exact request returns the same transaction and its latest known status.
func (s *Simulator) Charge(request ChargeRequest) (ChargeResult, error) {
	request = normalizeChargeRequest(request)
	if err := validateChargeRequest(request); err != nil {
		return ChargeResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.charges[request.AttemptID]; ok {
		if existing.request != request {
			return ChargeResult{}, ErrIdempotencyConflict
		}
		return existing.result, nil
	}

	result := ChargeResult{
		ProviderTransactionID: stableProviderID("pay", request.AttemptID+"\x00"+request.OrderID),
		Status:                request.InitialStatus,
	}
	s.charges[request.AttemptID] = chargeRecord{request: request, result: result}
	return result, nil
}

// Query returns the latest observed charge result for AttemptID.
func (s *Simulator) Query(attemptID string) (ChargeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.charges[attemptID]
	if !ok {
		return ChargeResult{}, ErrAttemptNotFound
	}
	return record.result, nil
}

// SetChargeQueryStatus simulates a later provider query. It can only resolve
// an UNKNOWN charge to SUCCESS; production code should not call it.
func (s *Simulator) SetChargeQueryStatus(attemptID string, status Status) error {
	if status != StatusSuccess {
		return ErrInvalidStatus
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.charges[attemptID]
	if !ok {
		return ErrAttemptNotFound
	}
	if record.result.Status != StatusUnknown {
		return ErrNotUnknown
	}
	record.result.Status = status
	s.charges[attemptID] = record
	return nil
}

// Refund creates at most one provider refund for a RefundRequestID. Repeating
// an exact request returns the same provider refund and latest known status.
func (s *Simulator) Refund(request RefundRequest) (RefundResult, error) {
	request = normalizeRefundRequest(request)
	if err := validateRefundRequest(request); err != nil {
		return RefundResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.refunds[request.RefundRequestID]; ok {
		if existing.request != request {
			return RefundResult{}, ErrIdempotencyConflict
		}
		return existing.result, nil
	}

	result := RefundResult{
		ProviderRefundID: stableProviderID("refund", request.RefundRequestID+"\x00"+request.ProviderTransactionID),
		Status:           request.InitialStatus,
	}
	s.refunds[request.RefundRequestID] = refundRecord{request: request, result: result}
	return result, nil
}

// QueryRefund returns the latest known result for a refund request.
func (s *Simulator) QueryRefund(refundRequestID string) (RefundResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.refunds[refundRequestID]
	if !ok {
		return RefundResult{}, ErrRefundNotFound
	}
	return record.result, nil
}

func stableProviderID(kind, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return kind + "_" + hex.EncodeToString(sum[:16])
}

func normalizeChargeRequest(request ChargeRequest) ChargeRequest {
	request.AttemptID = strings.TrimSpace(request.AttemptID)
	request.OrderID = strings.TrimSpace(request.OrderID)
	request.Currency = strings.ToUpper(strings.TrimSpace(request.Currency))
	return request
}

func normalizeRefundRequest(request RefundRequest) RefundRequest {
	request.RefundRequestID = strings.TrimSpace(request.RefundRequestID)
	request.ProviderTransactionID = strings.TrimSpace(request.ProviderTransactionID)
	request.Currency = strings.ToUpper(strings.TrimSpace(request.Currency))
	return request
}

func validateChargeRequest(request ChargeRequest) error {
	if request.AttemptID == "" {
		return ErrEmptyAttemptID
	}
	if request.Amount <= 0 {
		return ErrInvalidAmount
	}
	if request.Currency == "" {
		return ErrEmptyCurrency
	}
	if request.InitialStatus != StatusSuccess && request.InitialStatus != StatusUnknown {
		return ErrInvalidStatus
	}
	return nil
}

func validateRefundRequest(request RefundRequest) error {
	if request.RefundRequestID == "" {
		return ErrEmptyRefundID
	}
	if request.ProviderTransactionID == "" {
		return ErrEmptyProviderTxnID
	}
	if request.Amount <= 0 {
		return ErrInvalidAmount
	}
	if request.Currency == "" {
		return ErrEmptyCurrency
	}
	if request.InitialStatus != StatusSuccess && request.InitialStatus != StatusUnknown && request.InitialStatus != StatusFailedFinal {
		return ErrInvalidStatus
	}
	return nil
}
