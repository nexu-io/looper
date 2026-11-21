package loops

import "testing"

func TestProductSpecConfirmationRoundTripAndIdentity(t *testing.T) {
	existing := `{"awaitingProductSpec":true}`
	encoded, err := WriteProductSpecConfirmation(&existing, ProductSpecConfirmation{
		URL:          "https://docs.example/spec",
		PlaneActorID: "product-owner",
		ConfirmedAt:  "2026-07-16T04:00:00.000Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ProductSpecConfirmedBy(&encoded, "https://docs.example/spec", "product-owner") {
		t.Fatal("confirmation did not match its source URL and product identity")
	}
	if ProductSpecConfirmedBy(&encoded, "https://docs.example/other", "product-owner") || ProductSpecConfirmedBy(&encoded, "https://docs.example/spec", "looper-owner") {
		t.Fatal("confirmation must not transfer to another URL or actor")
	}
}
