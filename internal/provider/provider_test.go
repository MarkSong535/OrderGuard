package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseStatuses(t *testing.T) {
	if ParseFinancialStatus("PARTIALLY_REFUNDED") != FinancialPartiallyRefunded {
		t.Error("financial status not normalized")
	}
	if ParseFinancialStatus("NEW_ENUM_VALUE") != FinancialUnknown {
		t.Error("unknown financial status not mapped to unknown")
	}
	if ParseFulfillmentStatus("") != FulfillmentUnfulfilled {
		t.Error("empty fulfillment status should be unfulfilled")
	}
	if ParseFulfillmentStatus("IN_PROGRESS") != FulfillmentInProgress {
		t.Error("fulfillment status not normalized")
	}
	if ParseShipmentStatus("whatever") != ShipmentUnknown {
		t.Error("unknown shipment status not mapped to unknown")
	}
	if NormalizeDisplayStatus("") != "unknown" || NormalizeDisplayStatus("OUT_FOR_DELIVERY") != "out_for_delivery" {
		t.Error("display status not normalized")
	}
}

func TestValidate(t *testing.T) {
	valid := Order{ID: "1", Name: "#1", CreatedAt: time.Unix(1, 0), Total: Money{"10.00", "USD"},
		LineItems: []LineItem{{Title: "x", Quantity: 1}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	mutations := map[string]func(*Order){
		"no id":        func(o *Order) { o.ID = "" },
		"no name":      func(o *Order) { o.Name = "" },
		"no created":   func(o *Order) { o.CreatedAt = time.Time{} },
		"bad amount":   func(o *Order) { o.Total.Amount = "1e9" },
		"bad currency": func(o *Order) { o.Total.Currency = "usd" },
		"neg qty":      func(o *Order) { o.LineItems = []LineItem{{Title: "x", Quantity: -1}} },
		"no title":     func(o *Order) { o.LineItems = []LineItem{{Quantity: 1}} },
	}
	for name, mutate := range mutations {
		o := valid
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	base := &Error{Kind: KindRateLimited, Op: "x", RetryAfter: 3 * time.Second, Err: errors.New("cause")}
	wrapped := fmt.Errorf("outer: %w", base)
	if !IsRetryable(wrapped) || KindOf(wrapped) != KindRateLimited {
		t.Fatal("classification lost through wrapping")
	}
	if d, ok := RetryAfter(wrapped); !ok || d != 3*time.Second {
		t.Fatalf("RetryAfter = %v, %v", d, ok)
	}
	if IsRetryable(NotFound("x")) || IsRetryable(errors.New("plain")) {
		t.Fatal("non-retryable error classified as retryable")
	}
	if !strings.Contains(base.Error(), "rate_limited") {
		t.Fatalf("Error() = %q", base.Error())
	}
}
