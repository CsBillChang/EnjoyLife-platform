package order

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVoucherReissueAndRecoveryValidateBeforeDatabase(t *testing.T) {
	service := NewFulfillmentService(nil)
	for _, ids := range [][2]uint64{{0, 1}, {1, 0}} {
		if _, err := service.ReissueOwnedVoucher(context.Background(), ids[0], ids[1], time.Now()); !errors.Is(err, ErrInvalidFulfillmentInput) {
			t.Fatal(err)
		}
	}
	if _, err := service.ReissueOwnedVoucher(context.Background(), 1, 1, time.Time{}); !errors.Is(err, ErrInvalidFulfillmentInput) {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 0, MaxFulfillmentRecoveryBatch + 1} {
		if _, err := service.ListExpiredOrderIDs(context.Background(), 0, limit, time.Now()); !errors.Is(err, ErrInvalidFulfillmentInput) {
			t.Fatal(err)
		}
		if _, err := service.ListPendingRefunds(context.Background(), 0, limit); !errors.Is(err, ErrInvalidFulfillmentInput) {
			t.Fatal(err)
		}
	}
}

func TestOperationKeyMatchesPaymentDomainValidation(t *testing.T) {
	for _, key := range []string{"", "key ", "key\t", "中文", strings.Repeat("x", 129), "ok-key", strings.Repeat("x", 128)} {
		input := validPaymentFixture()
		input.AttemptID = key
		if (ValidateOperationKey(key) == nil) != (validateSuccessfulPayment(input) == nil) {
			t.Fatalf("HTTP/domain identity mismatch for %q", key)
		}
	}
}
