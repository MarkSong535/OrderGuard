// Package shopify implements provider.Provider on top of the Shopify Admin
// GraphQL API.
package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"orderguard/internal/provider"
)

const (
	op                      = "shopify.GetOrder"
	listOp                  = "shopify.ListOrders"
	defaultMaxResponseBytes = 1 << 20 // 1 MiB; an order payload is a few KiB
	userAgent               = "OrderGuard/1.0"
)

// Config configures a Client.
type Config struct {
	// ShopDomain is the shop's myshopify.com domain, e.g. "acme.myshopify.com".
	ShopDomain string
	// APIVersion is the Admin API version, e.g. "2025-07".
	APIVersion string
	// AccessToken is the Admin API access token. It is sent only in the
	// X-Shopify-Access-Token header and never logged.
	AccessToken string
	// Endpoint overrides the derived GraphQL URL (used for the local fake
	// server). Plain http is rejected unless AllowInsecureEndpoint is set.
	Endpoint              string
	AllowInsecureEndpoint bool
	// HTTPClient defaults to a pooled client from NewHTTPClient. Callers
	// should leave its Timeout at zero and rely on per-request contexts.
	HTTPClient       *http.Client
	MaxResponseBytes int64
}

// Client is a Shopify order provider. It is safe for concurrent use.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	maxBody  int64
}

var (
	shopDomainPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.myshopify\.com$`)
	apiVersionPattern = regexp.MustCompile(`^\d{4}-\d{2}$`)
)

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.AccessToken == "" {
		return nil, errors.New("shopify: access token is required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if !shopDomainPattern.MatchString(cfg.ShopDomain) {
			return nil, fmt.Errorf("shopify: invalid shop domain %q (want <shop>.myshopify.com)", cfg.ShopDomain)
		}
		if !apiVersionPattern.MatchString(cfg.APIVersion) {
			return nil, fmt.Errorf("shopify: invalid API version %q (want YYYY-MM)", cfg.APIVersion)
		}
		endpoint = "https://" + cfg.ShopDomain + "/admin/api/" + cfg.APIVersion + "/graphql.json"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("shopify: invalid endpoint %q", endpoint)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && cfg.AllowInsecureEndpoint:
	default:
		return nil, fmt.Errorf("shopify: endpoint must use https (got %q)", u.Scheme)
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = NewHTTPClient()
	}
	maxBody := cfg.MaxResponseBytes
	if maxBody <= 0 {
		maxBody = defaultMaxResponseBytes
	}
	return &Client{endpoint: endpoint, token: cfg.AccessToken, http: hc, maxBody: maxBody}, nil
}

// NewHTTPClient returns an HTTP client with connection pooling tuned for a
// single upstream host. Deadlines come from request contexts.
func NewHTTPClient() *http.Client {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: t,
		// Never follow redirects: they would carry the access token to
		// another location and Shopify's API does not use them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "shopify" }

// GetOrder implements provider.Provider.
func (c *Client) GetOrder(ctx context.Context, id string) (*provider.Order, error) {
	gr, err := c.query(ctx, GraphQLRequest{Query: orderQuery, Variables: map[string]any{"id": OrderGID(id)}})
	if err != nil {
		return nil, err
	}
	if gr.Data.Order == nil {
		return nil, provider.NotFound(op)
	}

	order, err := normalize(gr.Data.Order)
	if err != nil {
		return nil, &provider.Error{Kind: provider.KindBadResponse, Op: op, Err: err}
	}
	if order.ID != id {
		return nil, &provider.Error{Kind: provider.KindBadResponse, Op: op,
			Err: fmt.Errorf("response is for order %q, requested %q", order.ID, id)}
	}
	return order, nil
}

var customerIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// ListOrders implements provider.Provider.
func (c *Client) ListOrders(ctx context.Context, f provider.ListFilter) ([]*provider.Order, error) {
	vars := map[string]any{"first": f.Clamped()}
	if f.CustomerID != "" {
		// Never interpolate anything but a validated numeric ID into Shopify's search syntax.
		if !customerIDPattern.MatchString(f.CustomerID) {
			return nil, &provider.Error{Kind: provider.KindUnknown, Op: listOp, Err: errors.New("invalid customer id")}
		}
		vars["query"] = "customer_id:" + f.CustomerID
	}
	gr, err := c.query(ctx, GraphQLRequest{Query: ordersQuery, Variables: vars})
	if err != nil {
		return nil, err
	}
	if gr.Data.Orders == nil {
		return nil, &provider.Error{Kind: provider.KindBadResponse, Op: listOp, Err: errors.New("response has no orders")}
	}
	out := make([]*provider.Order, 0, len(gr.Data.Orders.Nodes))
	for i := range gr.Data.Orders.Nodes {
		o, err := normalize(&gr.Data.Orders.Nodes[i])
		if err != nil {
			return nil, &provider.Error{Kind: provider.KindBadResponse, Op: listOp, Err: fmt.Errorf("order %d: %w", i, err)}
		}
		if f.CustomerID != "" && o.CustomerID != f.CustomerID {
			return nil, &provider.Error{Kind: provider.KindBadResponse, Op: listOp,
				Err: fmt.Errorf("order %s does not belong to the requested customer", o.ID)}
		}
		out = append(out, o)
	}
	return out, nil
}

// query performs one GraphQL request and classifies every failure mode.
// A nil error guarantees a non-nil Data.
func (c *Client) query(ctx context.Context, q GraphQLRequest) (*GraphQLResponse, error) {
	body, err := json.Marshal(q)
	if err != nil {
		return nil, &provider.Error{Kind: provider.KindUnknown, Op: op, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.KindUnknown, Op: op, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Shopify-Access-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()

	if err := statusError(resp); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(raw)) > c.maxBody {
		return nil, &provider.Error{Kind: provider.KindBadResponse, Op: op, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("response exceeds %d bytes", c.maxBody)}
	}

	var gr GraphQLResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, &provider.Error{Kind: provider.KindBadResponse, Op: op, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("decode response: %w", err)}
	}
	if len(gr.Errors) > 0 {
		return nil, graphQLError(gr)
	}
	if gr.Data == nil {
		return nil, &provider.Error{Kind: provider.KindBadResponse, Op: op, Err: errors.New("response has no data")}
	}
	return &gr, nil
}

func transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return err // the caller gave up; not an upstream fault
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return &provider.Error{Kind: provider.KindTimeout, Op: op, Err: err}
	default:
		return &provider.Error{Kind: provider.KindUnavailable, Op: op, Err: err}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func statusError(resp *http.Response) error {
	code := resp.StatusCode
	switch {
	case code == http.StatusOK:
		return nil
	case code == http.StatusTooManyRequests:
		return &provider.Error{Kind: provider.KindRateLimited, Op: op, StatusCode: code,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return &provider.Error{Kind: provider.KindUnauthorized, Op: op, StatusCode: code}
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		return &provider.Error{Kind: provider.KindTimeout, Op: op, StatusCode: code}
	case code >= 500:
		return &provider.Error{Kind: provider.KindUnavailable, Op: op, StatusCode: code,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	default:
		// 404 here means the shop or API version does not exist: a
		// configuration problem, not a missing order.
		return &provider.Error{Kind: provider.KindBadResponse, Op: op, StatusCode: code}
	}
}

// parseRetryAfter accepts delta-seconds or an HTTP date. It returns 0 if absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 && secs < 3600 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

func graphQLError(gr GraphQLResponse) error {
	msgs := make([]string, 0, len(gr.Errors))
	kind := provider.KindBadResponse
	for _, e := range gr.Errors {
		msgs = append(msgs, e.Message)
		switch e.Extensions.Code {
		case "THROTTLED":
			kind = provider.KindRateLimited
		case "ACCESS_DENIED":
			if kind != provider.KindRateLimited {
				kind = provider.KindUnauthorized
			}
		case "INTERNAL_SERVER_ERROR":
			if kind == provider.KindBadResponse {
				kind = provider.KindUnavailable
			}
		}
	}
	pe := &provider.Error{Kind: kind, Op: op, Err: errors.New("graphql: " + strings.Join(msgs, "; "))}
	if kind == provider.KindRateLimited {
		pe.RetryAfter = throttleDelay(gr.Extensions)
	}
	return pe
}

// throttleDelay estimates how long until the cost bucket can afford the
// query: (requested - available) / restoreRate seconds, at least one second.
func throttleDelay(ext *ResponseExtras) time.Duration {
	const fallback = time.Second
	if ext == nil || ext.Cost == nil || ext.Cost.ThrottleStatus.RestoreRate <= 0 {
		return fallback
	}
	c := ext.Cost
	deficit := c.RequestedQueryCost - c.ThrottleStatus.CurrentlyAvailable
	if deficit <= 0 {
		return fallback
	}
	secs := math.Ceil(deficit / c.ThrottleStatus.RestoreRate)
	return max(time.Duration(secs)*time.Second, fallback)
}

func normalize(n *OrderNode) (*provider.Order, error) {
	id, err := ParseOrderGID(n.ID)
	if err != nil {
		return nil, err
	}
	o := &provider.Order{
		ID:                id,
		Name:              n.Name,
		Email:             deref(n.Email),
		FinancialStatus:   provider.ParseFinancialStatus(deref(n.DisplayFinancialStatus)),
		FulfillmentStatus: provider.ParseFulfillmentStatus(n.DisplayFulfillmentStatus),
		Total:             provider.Money{Amount: n.TotalPriceSet.ShopMoney.Amount, Currency: n.TotalPriceSet.ShopMoney.CurrencyCode},
	}
	if n.DisplayFinancialStatus == nil {
		o.FinancialStatus = provider.FinancialUnknown
	}
	if n.Customer != nil {
		if o.CustomerID, err = parseGID(n.Customer.ID, customerGIDPrefix); err != nil {
			return nil, err
		}
	}
	if o.CreatedAt, err = parseTime(n.CreatedAt, true); err != nil {
		return nil, fmt.Errorf("createdAt: %w", err)
	}
	if o.UpdatedAt, err = parseTime(n.UpdatedAt, false); err != nil {
		return nil, fmt.Errorf("updatedAt: %w", err)
	}

	o.LineItems = make([]provider.LineItem, 0, len(n.LineItems.Nodes))
	for _, li := range n.LineItems.Nodes {
		o.LineItems = append(o.LineItems, provider.LineItem{Title: li.Title, SKU: deref(li.SKU), Quantity: li.Quantity})
	}

	o.Fulfillments = make([]provider.Fulfillment, 0, len(n.Fulfillments))
	for i, f := range n.Fulfillments {
		pf := provider.Fulfillment{
			Status:        provider.ParseShipmentStatus(f.Status),
			DisplayStatus: provider.NormalizeDisplayStatus(deref(f.DisplayStatus)),
		}
		if pf.CreatedAt, err = parseTime(f.CreatedAt, false); err != nil {
			return nil, fmt.Errorf("fulfillment %d createdAt: %w", i, err)
		}
		if pf.UpdatedAt, err = parseTime(f.UpdatedAt, false); err != nil {
			return nil, fmt.Errorf("fulfillment %d updatedAt: %w", i, err)
		}
		for _, t := range f.TrackingInfo {
			pf.Tracking = append(pf.Tracking, provider.Tracking{
				Company: deref(t.Company), Number: deref(t.Number), URL: deref(t.URL),
			})
		}
		o.Fulfillments = append(o.Fulfillments, pf)
	}

	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("invalid order payload: %w", err)
	}
	return o, nil
}

func parseTime(s string, required bool) (time.Time, error) {
	if s == "" {
		if required {
			return time.Time{}, errors.New("missing timestamp")
		}
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
