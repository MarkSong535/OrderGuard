// Package provider defines the boundary between OrderGuard and external
// commerce platforms: a normalized order model, the Provider interface and a
// typed error taxonomy that the resilience layer uses to decide what is safe
// to retry.
package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Provider fetches orders from an external system of record.
//
// Implementations must honour ctx cancellation, must return *Error for
// upstream failures and must never fabricate fields the upstream did not
// supply: unknown values are surfaced as "unknown" or left empty.
type Provider interface {
	Name() string
	GetOrder(ctx context.Context, id string) (*Order, error)
	// ListOrders returns the most recent orders, newest first. A non-empty
	// CustomerID restricts the list to that customer's orders.
	ListOrders(ctx context.Context, f ListFilter) ([]*Order, error)
}

// ListFilter selects orders for ListOrders.
type ListFilter struct {
	CustomerID string // "" lists all orders
	Limit      int    // maximum number of orders; providers cap it
}

// MaxListLimit is the most orders a single ListOrders call returns.
const MaxListLimit = 50

// Clamped clamps f.Limit to [1, MaxListLimit], defaulting to MaxListLimit.
func (f ListFilter) Clamped() int {
	if f.Limit <= 0 || f.Limit > MaxListLimit {
		return MaxListLimit
	}
	return f.Limit
}

// Order is the provider-neutral order representation. Values returned by a
// Provider are treated as immutable because they may be shared between
// concurrent requests and the stale cache.
type Order struct {
	ID                string
	Name              string // human-facing order number, e.g. "#1001"
	CustomerID        string // empty for guest checkouts
	Email             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	FinancialStatus   FinancialStatus
	FulfillmentStatus FulfillmentStatus
	Total             Money
	LineItems         []LineItem
	Fulfillments      []Fulfillment
}

// Money is a decimal amount kept as a string to avoid float rounding.
type Money struct {
	Amount   string
	Currency string
}

// LineItem is one purchased product.
type LineItem struct {
	Title    string
	SKU      string
	Quantity int
}

// Fulfillment is one shipment of some or all of an order's items.
type Fulfillment struct {
	Status        ShipmentStatus
	DisplayStatus string // carrier-level status such as "in_transit", or "unknown"
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Tracking      []Tracking
}

// Tracking is carrier tracking information.
type Tracking struct {
	Company string
	Number  string
	URL     string
}

// FinancialStatus is the payment state of an order.
type FinancialStatus string

const (
	FinancialPending           FinancialStatus = "pending"
	FinancialAuthorized        FinancialStatus = "authorized"
	FinancialPartiallyPaid     FinancialStatus = "partially_paid"
	FinancialPaid              FinancialStatus = "paid"
	FinancialPartiallyRefunded FinancialStatus = "partially_refunded"
	FinancialRefunded          FinancialStatus = "refunded"
	FinancialVoided            FinancialStatus = "voided"
	FinancialExpired           FinancialStatus = "expired"
	FinancialUnknown           FinancialStatus = "unknown"
)

// FulfillmentStatus is the shipping state of an order as a whole.
type FulfillmentStatus string

const (
	FulfillmentUnfulfilled        FulfillmentStatus = "unfulfilled"
	FulfillmentPartiallyFulfilled FulfillmentStatus = "partially_fulfilled"
	FulfillmentFulfilled          FulfillmentStatus = "fulfilled"
	FulfillmentInProgress         FulfillmentStatus = "in_progress"
	FulfillmentOnHold             FulfillmentStatus = "on_hold"
	FulfillmentScheduled          FulfillmentStatus = "scheduled"
	FulfillmentPending            FulfillmentStatus = "pending_fulfillment"
	FulfillmentOpen               FulfillmentStatus = "open"
	FulfillmentRestocked          FulfillmentStatus = "restocked"
	FulfillmentRequestDeclined    FulfillmentStatus = "request_declined"
	FulfillmentUnknown            FulfillmentStatus = "unknown"
)

// ShipmentStatus is the state of an individual fulfillment.
type ShipmentStatus string

const (
	ShipmentPending   ShipmentStatus = "pending"
	ShipmentOpen      ShipmentStatus = "open"
	ShipmentSuccess   ShipmentStatus = "success"
	ShipmentCancelled ShipmentStatus = "cancelled"
	ShipmentError     ShipmentStatus = "error"
	ShipmentFailure   ShipmentStatus = "failure"
	ShipmentUnknown   ShipmentStatus = "unknown"
)

var (
	financialStatuses = setOf(FinancialPending, FinancialAuthorized, FinancialPartiallyPaid, FinancialPaid,
		FinancialPartiallyRefunded, FinancialRefunded, FinancialVoided, FinancialExpired)
	fulfillmentStatuses = setOf(FulfillmentUnfulfilled, FulfillmentPartiallyFulfilled, FulfillmentFulfilled,
		FulfillmentInProgress, FulfillmentOnHold, FulfillmentScheduled, FulfillmentPending, FulfillmentOpen,
		FulfillmentRestocked, FulfillmentRequestDeclined)
	shipmentStatuses = setOf(ShipmentPending, ShipmentOpen, ShipmentSuccess, ShipmentCancelled, ShipmentError, ShipmentFailure)
)

func setOf[T ~string](vs ...T) map[T]bool {
	m := make(map[T]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// enumKey converts an upstream enum such as "PARTIALLY_FULFILLED" to the
// normalized form "partially_fulfilled".
func enumKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ParseFinancialStatus maps an upstream value; unrecognised values become unknown.
func ParseFinancialStatus(s string) FinancialStatus {
	if v := FinancialStatus(enumKey(s)); financialStatuses[v] {
		return v
	}
	return FinancialUnknown
}

// ParseFulfillmentStatus maps an upstream value; unrecognised values become
// unknown. A missing value means nothing has shipped yet.
func ParseFulfillmentStatus(s string) FulfillmentStatus {
	if strings.TrimSpace(s) == "" {
		return FulfillmentUnfulfilled
	}
	if v := FulfillmentStatus(enumKey(s)); fulfillmentStatuses[v] {
		return v
	}
	return FulfillmentUnknown
}

// ParseShipmentStatus maps an upstream value; unrecognised values become unknown.
func ParseShipmentStatus(s string) ShipmentStatus {
	if v := ShipmentStatus(enumKey(s)); shipmentStatuses[v] {
		return v
	}
	return ShipmentUnknown
}

// NormalizeDisplayStatus lower-cases a free-form carrier status, mapping empty to unknown.
func NormalizeDisplayStatus(s string) string {
	if k := enumKey(s); k != "" {
		return k
	}
	return "unknown"
}

var (
	amountPattern   = regexp.MustCompile(`^-?\d{1,15}(\.\d{1,6})?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Validate checks that an order is complete enough to show to a customer.
// It exists so that a partial or malformed upstream payload is rejected
// rather than rendered with invented defaults.
func (o *Order) Validate() error {
	var problems []string
	if o.ID == "" {
		problems = append(problems, "missing id")
	}
	if o.Name == "" {
		problems = append(problems, "missing name")
	}
	if o.CreatedAt.IsZero() {
		problems = append(problems, "missing created_at")
	}
	if !amountPattern.MatchString(o.Total.Amount) {
		problems = append(problems, fmt.Sprintf("invalid total amount %q", o.Total.Amount))
	}
	if !currencyPattern.MatchString(o.Total.Currency) {
		problems = append(problems, fmt.Sprintf("invalid currency %q", o.Total.Currency))
	}
	for i, li := range o.LineItems {
		if li.Quantity < 0 {
			problems = append(problems, fmt.Sprintf("line item %d has negative quantity", i))
		}
		if li.Title == "" {
			problems = append(problems, fmt.Sprintf("line item %d missing title", i))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
