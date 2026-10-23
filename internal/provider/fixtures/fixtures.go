// Package fixtures contains clearly synthetic sample orders used by the
// in-memory provider, the fake Shopify server, tests and demos. None of this
// data describes real people or purchases.
package fixtures

import (
	"time"

	"orderguard/internal/provider"
)

// Customer IDs used in fixtures.
const (
	CustomerAlice = "7001"
	CustomerBob   = "7002"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Orders returns a fresh copy of the fixture orders.
func Orders() []provider.Order {
	return []provider.Order{
		{
			ID:                "1001",
			Name:              "#1001",
			CustomerID:        CustomerAlice,
			Email:             "alice@example.com",
			CreatedAt:         ts("2026-09-28T14:03:11Z"),
			UpdatedAt:         ts("2026-10-02T09:15:00Z"),
			FinancialStatus:   provider.FinancialPaid,
			FulfillmentStatus: provider.FulfillmentPartiallyFulfilled,
			Total:             provider.Money{Amount: "89.97", Currency: "USD"},
			LineItems: []provider.LineItem{
				{Title: "Trail Running Shoe", SKU: "TRS-42-BLK", Quantity: 1},
				{Title: "Merino Running Socks", SKU: "MRS-M-GRY", Quantity: 2},
			},
			Fulfillments: []provider.Fulfillment{{
				Status:        provider.ShipmentSuccess,
				DisplayStatus: "in_transit",
				CreatedAt:     ts("2026-09-30T08:00:00Z"),
				UpdatedAt:     ts("2026-10-02T09:15:00Z"),
				Tracking: []provider.Tracking{{
					Company: "Example Parcel",
					Number:  "EX123456789US",
					URL:     "https://tracking.example.com/EX123456789US",
				}},
			}},
		},
		{
			ID:                "1002",
			Name:              "#1002",
			CustomerID:        CustomerBob,
			Email:             "bob@example.com",
			CreatedAt:         ts("2026-10-01T18:42:05Z"),
			UpdatedAt:         ts("2026-10-05T16:20:00Z"),
			FinancialStatus:   provider.FinancialPaid,
			FulfillmentStatus: provider.FulfillmentFulfilled,
			Total:             provider.Money{Amount: "24.50", Currency: "USD"},
			LineItems: []provider.LineItem{
				{Title: "Insulated Water Bottle", SKU: "IWB-750-TEAL", Quantity: 1},
			},
			Fulfillments: []provider.Fulfillment{{
				Status:        provider.ShipmentSuccess,
				DisplayStatus: "delivered",
				CreatedAt:     ts("2026-10-02T10:00:00Z"),
				UpdatedAt:     ts("2026-10-05T16:20:00Z"),
				Tracking: []provider.Tracking{{
					Company: "Example Parcel",
					Number:  "EX987654321US",
					URL:     "https://tracking.example.com/EX987654321US",
				}},
			}},
		},
		{
			ID:                "1003",
			Name:              "#1003",
			CustomerID:        CustomerAlice,
			Email:             "alice@example.com",
			CreatedAt:         ts("2026-10-20T11:30:00Z"),
			UpdatedAt:         ts("2026-10-20T11:30:00Z"),
			FinancialStatus:   provider.FinancialAuthorized,
			FulfillmentStatus: provider.FulfillmentUnfulfilled,
			Total:             provider.Money{Amount: "149.00", Currency: "USD"},
			LineItems: []provider.LineItem{
				{Title: "Ultralight Rain Jacket", SKU: "URJ-L-NVY", Quantity: 1},
			},
		},
		{
			ID:                "1004",
			Name:              "#1004",
			Email:             "guest@example.com", // guest checkout: no customer account
			CreatedAt:         ts("2026-10-21T07:05:42Z"),
			UpdatedAt:         ts("2026-10-21T07:05:42Z"),
			FinancialStatus:   provider.FinancialPending,
			FulfillmentStatus: provider.FulfillmentOnHold,
			Total:             provider.Money{Amount: "12.00", Currency: "EUR"},
			LineItems: []provider.LineItem{
				{Title: "Gift Card", SKU: "", Quantity: 1},
			},
		},
	}
}
