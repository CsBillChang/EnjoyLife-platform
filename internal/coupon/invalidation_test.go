package coupon

import "testing"

func TestInvalidationPayloadMustMatchTaskGeneration(t *testing.T) {
	p := couponInvalidationPayload{ActivityID: 1, ShopID: 2, Generation: 9}
	if !validInvalidationPayload(p, 9) {
		t.Fatal("valid payload rejected")
	}
	if validInvalidationPayload(p, 8) {
		t.Fatal("mismatched task generation accepted")
	}
	if validInvalidationPayload(couponInvalidationPayload{Generation: 9}, 9) {
		t.Fatal("empty ids accepted")
	}
}
