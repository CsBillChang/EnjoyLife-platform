package order

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func asyncRuleFixture() (PurchaseRequest, asyncActivity) {
	accepted := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	digest := purchaseParamsDigest(2)
	request := PurchaseRequest{ID: 1, RequestID: "3f590b98-40c1-4bcb-9a8a-998e1c99c667",
		UserID: 1, ActivityID: 2, Scope: PurchaseScope, IdempotencyKey: "key", ParamsDigest: digest[:],
		AcceptedAt: accepted, ProcessingDeadline: accepted.Add(ProcessingWindow), ShopID: 3,
		PriceCents: 100, Currency: "CNY", Generation: 1, Status: RequestAdmitted}
	activity := asyncActivity{purchaseActivity: purchaseActivity{ID: 2, ShopID: 3, PriceCents: 100,
		Currency: "CNY", StartsAt: accepted.Add(-time.Hour), EndsAt: accepted.Add(time.Minute),
		AdmissionState: "OPEN", AvailableStock: 1}, Generation: 1}
	return request, activity
}

func TestAsyncAcceptanceInputAndUserScope(t *testing.T) {
	request, _ := asyncRuleFixture()
	if err := validateAsyncAcceptance(1, 2, strings.Repeat("k", 128), request.AcceptedAt); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "trailing ", "line\n", "中文", strings.Repeat("k", 129)} {
		if err := validateAsyncAcceptance(1, 2, key, request.AcceptedAt); !errors.Is(err, ErrInvalidPurchaseInput) {
			t.Fatalf("invalid key accepted: %v", err)
		}
	}
	for _, ids := range [][2]uint64{{0, 1}, {1, 0}} {
		if err := validateAsyncAcceptance(ids[0], ids[1], "key", request.AcceptedAt); !errors.Is(err, ErrInvalidPurchaseInput) {
			t.Fatalf("invalid IDs accepted: %v", err)
		}
	}
	for _, now := range []time.Time{time.Time{}, time.Date(9999, 12, 31, 23, 59, 0, 0, time.UTC)} {
		if err := validateAsyncAcceptance(1, 2, "key", now); !errors.Is(err, ErrInvalidPurchaseInput) {
			t.Fatalf("invalid timestamp accepted: %v", err)
		}
	}
	digest := purchaseParamsDigest(request.ActivityID)
	for _, state := range []RequestStatus{RequestReceived, RequestAdmitted, RequestSucceeded, RequestFailed} {
		request.Status = state
		got, err := matchAcceptedRequest(request, request.ActivityID, digest[:])
		if err != nil || got.RequestID != request.RequestID || got.Status != state {
			t.Fatalf("replay changed recorded request: %+v err=%v", got, err)
		}
	}
	changedDigest := purchaseParamsDigest(999)
	if digest == changedDigest {
		t.Fatal("different activities have identical parameter digests")
	}
	for _, change := range []func(*PurchaseRequest){
		func(p *PurchaseRequest) { p.ActivityID++ },
		func(p *PurchaseRequest) { p.ParamsDigest = changedDigest[:] },
		func(p *PurchaseRequest) { p.Scope = "OTHER" },
	} {
		changed := request
		change(&changed)
		if _, err := matchAcceptedRequest(changed, request.ActivityID, digest[:]); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("mismatched request replay error=%v", err)
		}
	}
}

func TestAsyncAcceptedTimeAndProcessingDeadlineAreIndependent(t *testing.T) {
	request, activity := asyncRuleFixture()
	// The sale has ended now, but acceptance was valid and its own processing
	// deadline has not elapsed. Reusing a current-time sale check would fail this.
	afterSale := activity.EndsAt.Add(time.Second)
	if failure, err := validateRequestProcessing(request, activity, afterSale); err != nil || failure != "" {
		t.Fatalf("valid accepted request rejected after sale: failure=%s err=%v", failure, err)
	}
	if failure, err := validateRequestProcessing(request, activity, request.ProcessingDeadline.Add(-time.Microsecond)); err != nil || failure != "" {
		t.Fatalf("before processing deadline: failure=%s err=%v", failure, err)
	}
	for _, now := range []time.Time{request.ProcessingDeadline, request.ProcessingDeadline.Add(time.Second)} {
		if failure, err := validateRequestProcessing(request, activity, now); err != nil || failure != FailureProcessingExpired {
			t.Fatalf("processing expiration: failure=%s err=%v", failure, err)
		}
	}
	request.Status = RequestReceived
	if _, err := validateRequestProcessing(request, activity, request.AcceptedAt); !errors.Is(err, ErrRequestNotAdmitted) {
		t.Fatalf("unadmitted request was accepted: %v", err)
	}
	if failure, err := validateRequestProcessing(request, activity, request.ProcessingDeadline); err != nil || failure != FailureProcessingExpired {
		t.Fatalf("expired RECEIVED should become a queryable failure: %s / %v", failure, err)
	}
}

func TestAsyncAcceptedSnapshotValidation(t *testing.T) {
	request, activity := asyncRuleFixture()
	for _, accepted := range []time.Time{activity.StartsAt.Add(-time.Microsecond), activity.EndsAt} {
		changed := request
		changed.AcceptedAt = accepted
		if failure := validateAcceptedSnapshot(changed, activity); failure != FailureInvalidAcceptedAt {
			t.Fatalf("invalid acceptance time: failure=%s", failure)
		}
	}
	for _, accepted := range []time.Time{activity.StartsAt, activity.EndsAt.Add(-time.Microsecond)} {
		changed := request
		changed.AcceptedAt = accepted
		if failure := validateAcceptedSnapshot(changed, activity); failure != "" {
			t.Fatalf("valid acceptance boundary: failure=%s", failure)
		}
	}
	for name, change := range map[string]func(*PurchaseRequest){
		"activity": func(p *PurchaseRequest) { p.ActivityID++ },
		"shop":     func(p *PurchaseRequest) { p.ShopID++ },
		"price":    func(p *PurchaseRequest) { p.PriceCents++ },
		"currency": func(p *PurchaseRequest) { p.Currency = "USD" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			change(&changed)
			if failure := validateAcceptedSnapshot(changed, activity); failure != FailureActivitySnapshotChanged {
				t.Fatalf("snapshot mismatch: failure=%s", failure)
			}
		})
	}
}

func TestAsyncExistingOrderMustMatchRequest(t *testing.T) {
	request, _ := asyncRuleFixture()
	item := Order{RequestID: request.RequestID, UserID: request.UserID, ActivityID: request.ActivityID,
		ShopID: request.ShopID, PriceCents: request.PriceCents, Currency: request.Currency, IdempotencyKey: request.IdempotencyKey}
	if !orderMatchesRequest(item, request) {
		t.Fatal("matching order was rejected")
	}
	for _, change := range []func(*Order){
		func(o *Order) { o.RequestID = "other" },
		func(o *Order) { o.UserID++ },
		func(o *Order) { o.ActivityID++ },
		func(o *Order) { o.ShopID++ },
		func(o *Order) { o.PriceCents++ },
		func(o *Order) { o.Currency = "USD" },
		func(o *Order) { o.IdempotencyKey = "other" },
	} {
		changed := item
		change(&changed)
		if orderMatchesRequest(changed, request) {
			t.Fatal("unrelated order could be used as request success")
		}
	}
}

func TestAsyncUUIDAndEventPayload(t *testing.T) {
	id, err := newRequestID()
	if err != nil || !validRequestUUID(id) {
		t.Fatalf("generated request UUID is invalid: %v", err)
	}
	for _, invalid := range []string{"", "not-a-uuid", strings.Repeat("a", 36), "3F590B98-40C1-4BCB-9A8A-998E1C99C667", id[:35], id + "x", "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"} {
		if validRequestUUID(invalid) {
			t.Fatalf("invalid request UUID accepted: %q", invalid)
		}
	}
	payload, err := purchaseEventPayload(id)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(payload, &fields); err != nil || len(fields) != 1 || fields["request_id"] != id {
		t.Fatalf("event payload=%s err=%v", payload, err)
	}
}

func TestAsyncTerminalAndInputChecks(t *testing.T) {
	for _, state := range []RequestStatus{RequestSucceeded, RequestFailed} {
		if !state.Terminal() {
			t.Fatalf("terminal state %s not recognized", state)
		}
	}
	for _, state := range []RequestStatus{RequestReceived, RequestAdmitted, ""} {
		if state.Terminal() {
			t.Fatalf("nonterminal state %s recognized as final", state)
		}
	}
	service := NewAsyncService(nil)
	if _, err := service.Accept(context.Background(), 0, 1, "key", time.Now()); !errors.Is(err, ErrInvalidPurchaseInput) {
		t.Fatalf("accept input check=%v", err)
	}
	if _, err := service.Process(context.Background(), "bad", time.Now()); !errors.Is(err, ErrInvalidPurchaseInput) {
		t.Fatalf("process input check=%v", err)
	}
	if _, err := service.GetOwnedRequest(context.Background(), 1, "bad"); !errors.Is(err, ErrInvalidPurchaseInput) {
		t.Fatalf("owned request input check=%v", err)
	}
	request, _ := asyncRuleFixture()
	if _, err := service.GetOwnedRequest(context.Background(), 0, request.RequestID); !errors.Is(err, ErrInvalidPurchaseInput) {
		t.Fatalf("owned request user check=%v", err)
	}
}

func TestAsyncDBErrorPreservesRetryability(t *testing.T) {
	for _, code := range []uint16{1205, 1213} {
		original := &mysql.MySQLError{Number: code}
		err := asyncDBError("process", original)
		if !errors.Is(err, ErrTransactionRetryable) {
			t.Fatalf("lost retryable code %d: %v", code, err)
		}
		var mysqlCause *mysql.MySQLError
		if !errors.As(err, &mysqlCause) || mysqlCause != original {
			t.Fatal("lost MySQL error cause")
		}
	}
	if err := asyncDBError("process", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost context cancellation: %v", err)
	}
}
