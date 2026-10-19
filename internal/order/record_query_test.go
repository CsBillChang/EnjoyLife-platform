package order

import (
	"errors"
	"testing"
	"time"
)

func TestRecordCursorRoundTripAndRejectsMalformedOrExtendedPayload(t *testing.T) {
	want := recordCursor{At: time.Date(2026, 9, 22, 8, 9, 10, 123456000, time.UTC), ID: 99}
	got, err := decodeRecordCursor(encodeRecordCursor(want))
	if err != nil || !got.At.Equal(want.At) || got.ID != want.ID {
		t.Fatalf("cursor=%+v err=%v", got, err)
	}
	for _, input := range []string{"", "not-base64", "e30", "eyJ0IjoiMjAyNi0wOS0yMlQwMDowMDowMFoiLCJpIjowfQ"} {
		if _, err := decodeRecordCursor(input); !errors.Is(err, ErrInvalidRecordPage) {
			t.Fatalf("input=%q error=%v", input, err)
		}
	}
}

func TestRecordPageUsesExtraRowForStableNextCursor(t *testing.T) {
	at := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	page := paymentPage([]PaymentRecord{{PaymentID: 3, CreatedAt: at.Add(2 * time.Second)}, {PaymentID: 2, CreatedAt: at}, {PaymentID: 1, CreatedAt: at}}, 2)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	cursor, err := decodeRecordCursor(page.NextCursor)
	if err != nil || cursor.ID != 2 || !cursor.At.Equal(at) {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
}
