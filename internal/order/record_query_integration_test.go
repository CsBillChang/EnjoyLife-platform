package order_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/order"
)

// This test runs only with the existing LOCAL_LIFE_INTEGRATION MySQL fixture.
// It verifies that authorization predicates are part of the actual joins, not
// only an HTTP convention or a fake-service expectation.
func TestMySQLRecordQueriesRespectOwnerAndMerchantShopMembership(t *testing.T) {
	db := fulfillmentTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fixture := newFulfillmentFixture(t, ctx, db)
	fulfillment := order.NewFulfillmentService(db)
	paid, err := fulfillment.ApplySuccessfulPayment(ctx, fixture.payment("record-query", fixture.now))
	if err != nil {
		t.Fatal(err)
	}
	queries := order.NewRecordQueryService(db)

	payments, err := queries.ListPaymentRecords(ctx, fixture.customerID, order.RecordPage{Limit: 20})
	if err != nil || len(payments.Items) != 1 || payments.Items[0].PaymentID != paid.PaymentID || payments.Items[0].OrderID != fixture.order.ID {
		t.Fatalf("owned payments=%+v err=%v", payments, err)
	}
	foreignPayments, err := queries.ListPaymentRecords(ctx, fixture.merchantID, order.RecordPage{Limit: 20})
	if err != nil || len(foreignPayments.Items) != 0 {
		t.Fatalf("foreign payments=%+v err=%v", foreignPayments, err)
	}

	if _, err := fulfillment.Redeem(ctx, fixture.merchantID, paid.VoucherCode, fixture.now); err != nil {
		t.Fatal(err)
	}
	redemptions, err := queries.ListMerchantRedemptionRecords(ctx, fixture.merchantID, fixture.shopID, order.RecordPage{Limit: 20})
	if err != nil || len(redemptions.Items) != 1 || redemptions.Items[0].OrderID != fixture.order.ID || redemptions.Items[0].MerchantUserID != fixture.merchantID {
		t.Fatalf("merchant redemptions=%+v err=%v", redemptions, err)
	}
	if _, err := queries.ListMerchantRedemptionRecords(ctx, fixture.customerID, fixture.shopID, order.RecordPage{Limit: 20}); !errors.Is(err, order.ErrMerchantShopNotFound) {
		t.Fatalf("unowned shop error=%v", err)
	}
}
