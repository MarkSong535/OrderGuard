package shopify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// orderFields selects only the fields OrderGuard exposes. Keeping the
// selection small keeps the GraphQL query cost (and throttling) low.
const orderFields = `fragment OrderFields on Order {
  id
  name
  email
  createdAt
  updatedAt
  displayFinancialStatus
  displayFulfillmentStatus
  customer { id }
  totalPriceSet { shopMoney { amount currencyCode } }
  lineItems(first: 50) { nodes { title sku quantity } }
  fulfillments(first: 20) {
    status
    displayStatus
    createdAt
    updatedAt
    trackingInfo(first: 5) { company number url }
  }
}`

const orderQuery = `query GetOrder($id: ID!) {
  order(id: $id) { ...OrderFields }
}
` + orderFields

// ordersQuery lists recent orders, optionally filtered with Shopify's search
// syntax (for example "customer_id:7001").
const ordersQuery = `query ListOrders($first: Int!, $query: String) {
  orders(first: $first, query: $query, sortKey: CREATED_AT, reverse: true) {
    nodes { ...OrderFields }
  }
}
` + orderFields

// The wire types below mirror the subset of the Shopify Admin GraphQL schema
// that orderQuery selects. They are exported so the fake server in
// shopifytest produces byte-compatible payloads.

// GraphQLRequest is a GraphQL POST body.
type GraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// GraphQLResponse is the envelope Shopify returns for orderQuery.
type GraphQLResponse struct {
	Data       *OrderData      `json:"data,omitempty"`
	Errors     GraphQLErrors   `json:"errors,omitempty"`
	Extensions *ResponseExtras `json:"extensions,omitempty"`
}

// OrderData is the data member of the response: Order for orderQuery,
// Orders for ordersQuery.
type OrderData struct {
	Order  *OrderNode       `json:"order,omitempty"`
	Orders *OrderConnection `json:"orders,omitempty"`
}

// OrderConnection is an OrderConnection.
type OrderConnection struct {
	Nodes []OrderNode `json:"nodes"`
}

// GraphQLError is a single GraphQL error.
type GraphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code,omitempty"`
	} `json:"extensions"`
}

// GraphQLErrors accepts both the standard GraphQL error array and the plain
// string Shopify returns for some transport-level failures (for example an
// invalid access token).
type GraphQLErrors []GraphQLError

// UnmarshalJSON implements json.Unmarshaler.
func (e *GraphQLErrors) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*e = GraphQLErrors{{Message: s}}
		return nil
	}
	var list []GraphQLError
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*e = list
	return nil
}

// ResponseExtras carries query cost information used for throttling.
type ResponseExtras struct {
	Cost *QueryCost `json:"cost,omitempty"`
}

// QueryCost is Shopify's calculated query cost and bucket state.
type QueryCost struct {
	RequestedQueryCost float64        `json:"requestedQueryCost"`
	ActualQueryCost    *float64       `json:"actualQueryCost"`
	ThrottleStatus     ThrottleStatus `json:"throttleStatus"`
}

// ThrottleStatus is the leaky-bucket state for the app.
type ThrottleStatus struct {
	MaximumAvailable   float64 `json:"maximumAvailable"`
	CurrentlyAvailable float64 `json:"currentlyAvailable"`
	RestoreRate        float64 `json:"restoreRate"`
}

// OrderNode is the Order object.
type OrderNode struct {
	ID                       string          `json:"id"`
	Name                     string          `json:"name"`
	Email                    *string         `json:"email"`
	CreatedAt                string          `json:"createdAt"`
	UpdatedAt                string          `json:"updatedAt"`
	DisplayFinancialStatus   *string         `json:"displayFinancialStatus"`
	DisplayFulfillmentStatus string          `json:"displayFulfillmentStatus"`
	Customer                 *CustomerNode   `json:"customer"`
	TotalPriceSet            MoneyBag        `json:"totalPriceSet"`
	LineItems                LineItemConn    `json:"lineItems"`
	Fulfillments             []FulfillmentNd `json:"fulfillments"`
}

// CustomerNode is the Customer object (id only).
type CustomerNode struct {
	ID string `json:"id"`
}

// MoneyBag is a MoneyBag object.
type MoneyBag struct {
	ShopMoney MoneyV2 `json:"shopMoney"`
}

// MoneyV2 is a MoneyV2 object.
type MoneyV2 struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currencyCode"`
}

// LineItemConn is a LineItemConnection.
type LineItemConn struct {
	Nodes []LineItemNode `json:"nodes"`
}

// LineItemNode is a LineItem object.
type LineItemNode struct {
	Title    string  `json:"title"`
	SKU      *string `json:"sku"`
	Quantity int     `json:"quantity"`
}

// FulfillmentNd is a Fulfillment object.
type FulfillmentNd struct {
	Status        string         `json:"status"`
	DisplayStatus *string        `json:"displayStatus"`
	CreatedAt     string         `json:"createdAt"`
	UpdatedAt     string         `json:"updatedAt"`
	TrackingInfo  []TrackingNode `json:"trackingInfo"`
}

// TrackingNode is a FulfillmentTrackingInfo object.
type TrackingNode struct {
	Company *string `json:"company"`
	Number  *string `json:"number"`
	URL     *string `json:"url"`
}

const (
	orderGIDPrefix    = "gid://shopify/Order/"
	customerGIDPrefix = "gid://shopify/Customer/"
)

// OrderGID converts a numeric order ID to a Shopify global ID.
func OrderGID(id string) string { return orderGIDPrefix + id }

// CustomerGID converts a numeric customer ID to a Shopify global ID.
func CustomerGID(id string) string { return customerGIDPrefix + id }

// ParseGID extracts the numeric ID from a global ID with the given prefix.
func parseGID(gid, prefix string) (string, error) {
	id, ok := strings.CutPrefix(gid, prefix)
	if !ok || id == "" {
		return "", fmt.Errorf("unexpected global id %q", gid)
	}
	return id, nil
}

// ParseOrderGID extracts the numeric ID from an order global ID.
func ParseOrderGID(gid string) (string, error) { return parseGID(gid, orderGIDPrefix) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
