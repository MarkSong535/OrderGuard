package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/provider"
)

// ErrorBody is the envelope for every non-2xx JSON response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes a failure. Code is stable and machine-readable;
// Message is for humans and may change.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// OrderResponse is the body of GET /orders/{id}.
type OrderResponse struct {
	Order OrderDTO `json:"order"`
	Meta  Meta     `json:"meta"`
}

// FulfillmentResponse is the body of GET /orders/{id}/fulfillment.
type FulfillmentResponse struct {
	OrderID           string           `json:"order_id"`
	Name              string           `json:"name"`
	FulfillmentStatus string           `json:"fulfillment_status"`
	Fulfillments      []FulfillmentDTO `json:"fulfillments"`
	Meta              Meta             `json:"meta"`
}

// Meta describes data provenance so clients (including AI agents) never
// mistake cached data for live data.
type Meta struct {
	Source    string    `json:"source"` // "live" or "stale"
	FetchedAt time.Time `json:"fetched_at"`
	RequestID string    `json:"request_id"`
}

// OrderDTO is the public order representation.
type OrderDTO struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	EmailMasked       string           `json:"email_masked,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         *time.Time       `json:"updated_at,omitempty"`
	FinancialStatus   string           `json:"financial_status"`
	FulfillmentStatus string           `json:"fulfillment_status"`
	Total             MoneyDTO         `json:"total"`
	LineItems         []LineItemDTO    `json:"line_items"`
	Fulfillments      []FulfillmentDTO `json:"fulfillments"`
}

// MoneyDTO is a decimal amount and ISO 4217 currency.
type MoneyDTO struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// LineItemDTO is a purchased product.
type LineItemDTO struct {
	Title    string `json:"title"`
	SKU      string `json:"sku,omitempty"`
	Quantity int    `json:"quantity"`
}

// FulfillmentDTO is one shipment.
type FulfillmentDTO struct {
	Status        string        `json:"status"`
	DisplayStatus string        `json:"display_status"`
	CreatedAt     *time.Time    `json:"created_at,omitempty"`
	UpdatedAt     *time.Time    `json:"updated_at,omitempty"`
	Tracking      []TrackingDTO `json:"tracking"`
}

// TrackingDTO is carrier tracking data.
type TrackingDTO struct {
	Company string `json:"company,omitempty"`
	Number  string `json:"number,omitempty"`
	URL     string `json:"url,omitempty"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func toOrderDTO(o *provider.Order) OrderDTO {
	dto := OrderDTO{
		ID:                o.ID,
		Name:              o.Name,
		EmailMasked:       maskEmail(o.Email),
		CreatedAt:         o.CreatedAt.UTC(),
		UpdatedAt:         optTime(o.UpdatedAt),
		FinancialStatus:   string(o.FinancialStatus),
		FulfillmentStatus: string(o.FulfillmentStatus),
		Total:             MoneyDTO{Amount: o.Total.Amount, Currency: o.Total.Currency},
		LineItems:         make([]LineItemDTO, 0, len(o.LineItems)),
		Fulfillments:      toFulfillmentDTOs(o.Fulfillments),
	}
	for _, li := range o.LineItems {
		dto.LineItems = append(dto.LineItems, LineItemDTO{Title: li.Title, SKU: li.SKU, Quantity: li.Quantity})
	}
	return dto
}

func toFulfillmentDTOs(fs []provider.Fulfillment) []FulfillmentDTO {
	out := make([]FulfillmentDTO, 0, len(fs))
	for _, f := range fs {
		d := FulfillmentDTO{
			Status:        string(f.Status),
			DisplayStatus: f.DisplayStatus,
			CreatedAt:     optTime(f.CreatedAt),
			UpdatedAt:     optTime(f.UpdatedAt),
			Tracking:      make([]TrackingDTO, 0, len(f.Tracking)),
		}
		for _, t := range f.Tracking {
			d.Tracking = append(d.Tracking, TrackingDTO{Company: t.Company, Number: t.Number, URL: t.URL})
		}
		out = append(out, d)
	}
	return out
}

func toMeta(r *orders.Result, requestID string) Meta {
	return Meta{Source: string(r.Source), FetchedAt: r.FetchedAt.UTC(), RequestID: requestID}
}

// SearchResponse is the body of GET /orders.
type SearchResponse struct {
	Results      []SearchHit `json:"results"`
	TotalMatches int         `json:"total_matches"` // before the limit
	Meta         Meta        `json:"meta"`
}

// SearchHit is one matching order. MatchedOn is empty when no query was given.
type SearchHit struct {
	Order     OrderDTO `json:"order"`
	MatchedOn []string `json:"matched_on"`
}

func toSearchResponse(r *orders.SearchResult, requestID string) SearchResponse {
	out := SearchResponse{
		Results:      make([]SearchHit, 0, len(r.Matches)),
		TotalMatches: r.Total,
		Meta:         Meta{Source: string(r.Source), FetchedAt: r.FetchedAt.UTC(), RequestID: requestID},
	}
	for _, m := range r.Matches {
		on := m.MatchedOn
		if on == nil {
			on = []string{}
		}
		out.Results = append(out.Results, SearchHit{Order: toOrderDTO(m.Order), MatchedOn: on})
	}
	return out
}

// maskEmail keeps enough of an address for a customer to recognise it
// ("a***@example.com") without exposing it in full to support tooling.
func maskEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return ""
	}
	return local[:1] + "***@" + domain
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: observability.RequestID(r.Context()),
	}})
}

func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d <= 0 {
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
}
