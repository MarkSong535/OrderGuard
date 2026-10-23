package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"orderguard/internal/auth"
	"orderguard/internal/observability"
)

// MCP (Model Context Protocol) endpoint: POST /mcp, Streamable HTTP
// transport, JSON responses only (no server-initiated streams), stateless
// (no Mcp-Session-Id). It exposes OrderGuard's read operations as tools so
// AI agents can use them. Every call is made as the principal of the bearer
// key, so the same authorization rules apply as for the REST API.

const mcpLatestVersion = "2025-06-18"

var mcpSupportedVersions = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func orderIDSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"order_id": map[string]any{"type": "string", "description": `The order number, e.g. "1002" or "#1002".`},
		},
		"required":             []string{"order_id"},
		"additionalProperties": false,
	}
}

var mcpTools = []mcpTool{
	{
		Name:  "search_orders",
		Title: "Search orders",
		Description: "Search the signed-in customer's orders (all orders for support staff). `query` is a case-insensitive " +
			"regular expression matched against the order number and the titles of the ordered items, e.g. \"bottle\", " +
			"\"jacket|shoe\" or \"^#100[12]$\". Matching is literal, so search for a short word stem (\"shoe\" also matches \"Shoes\"). " +
			"Leave it empty to list the most recent orders. Returns full order details, newest first.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Case-insensitive regular expression; empty lists all visible orders."},
				"limit": map[string]any{"type": "integer", "description": "Maximum number of orders to return (1-50, default 10)."},
			},
			"additionalProperties": false,
		},
		Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		Name:        "get_order",
		Title:       "Get order",
		Description: "Get one order by number: items, total, payment status, fulfillment status and shipments.",
		InputSchema: orderIDSchema(),
		Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		Name:        "get_order_tracking",
		Title:       "Get order tracking",
		Description: "Get shipping progress for one order: fulfillment status, carriers and tracking numbers.",
		InputSchema: orderIDSchema(),
		Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !sameOriginOrNone(r) { // DNS-rebinding protection required by the MCP transport spec
		writeError(w, r, http.StatusForbidden, "forbidden_origin", "cross-origin MCP requests are not allowed")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "this MCP endpoint only accepts POST")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{rpcParseError, "request body too large or unreadable"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{rpcParseError, "invalid JSON-RPC message"}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: idOrNull(req.ID), Error: &rpcError{rpcInvalidRequest, "invalid JSON-RPC request"}})
		return
	}
	if len(req.ID) == 0 { // notification (e.g. notifications/initialized): acknowledge without a body
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := mcpLatestVersion
		if mcpSupportedVersions[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "orderguard", "title": "OrderGuard", "version": "1.0.0"},
			"instructions":    "Order lookup tools. Results only include orders the caller is allowed to see; anything else is reported as not found.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{rpcInvalidParams, "invalid tools/call params"}
			break
		}
		result, rerr := s.callMCPTool(r, params.Name, params.Arguments)
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{rpcMethodNotFound, "method not found: " + req.Method}
	}
	writeRPC(w, resp)
}

// callMCPTool runs a tool. Protocol problems (unknown tool, bad arguments)
// are JSON-RPC errors; order lookup failures are tool results with isError,
// so the model can read them and react.
func (s *Server) callMCPTool(r *http.Request, name string, rawArgs json.RawMessage) (map[string]any, *rpcError) {
	p, _ := auth.PrincipalFrom(r.Context())
	ctx := r.Context()
	reqID := observability.RequestID(ctx)

	var args struct {
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		OrderID string `json:"order_id"`
	}
	if len(rawArgs) > 0 && string(rawArgs) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(rawArgs)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return nil, &rpcError{rpcInvalidParams, fmt.Sprintf("invalid arguments for %s: %v", name, err)}
		}
	}

	switch name {
	case "search_orders":
		res, err := s.orders.SearchOrders(ctx, p, args.Query, args.Limit)
		if err != nil {
			return s.mcpToolError(r, err), nil
		}
		return mcpToolResult(toSearchResponse(res, reqID)), nil
	case "get_order", "get_order_tracking":
		if args.OrderID == "" {
			return nil, &rpcError{rpcInvalidParams, "order_id is required"}
		}
		res, err := s.orders.GetOrder(ctx, p, normalizeOrderNumber(args.OrderID))
		if err != nil {
			return s.mcpToolError(r, err), nil
		}
		meta := toMeta(res, reqID)
		if name == "get_order" {
			return mcpToolResult(OrderResponse{Order: toOrderDTO(res.Order), Meta: meta}), nil
		}
		return mcpToolResult(FulfillmentResponse{
			OrderID: res.Order.ID, Name: res.Order.Name, FulfillmentStatus: string(res.Order.FulfillmentStatus),
			Fulfillments: toFulfillmentDTOs(res.Order.Fulfillments), Meta: meta,
		}), nil
	}
	return nil, &rpcError{rpcInvalidParams, "unknown tool: " + name}
}

// normalizeOrderNumber accepts "#1002" and surrounding spaces from agents;
// the order service still validates the result strictly.
func normalizeOrderNumber(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "#")
}

func mcpToolResult(v any) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(b)}},
		"structuredContent": json.RawMessage(b),
		"isError":           false,
	}
}

func (s *Server) mcpToolError(r *http.Request, err error) map[string]any {
	s.logOrderError(r, err)
	e := describeOrderError(err)
	detail := map[string]any{"code": e.Code, "message": e.Message}
	if e.RetryAfter > 0 {
		detail["retry_after_seconds"] = int(e.RetryAfter.Seconds() + 0.999)
	}
	b, _ := json.Marshal(map[string]any{"error": detail})
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": fmt.Sprintf("error: %s: %s", e.Code, e.Message)}},
		"structuredContent": json.RawMessage(b),
		"isError":           true,
	}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	writeJSON(w, http.StatusOK, resp)
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// sameOriginOrNone allows requests without an Origin header (server-side
// clients such as agents) and same-origin browser requests.
func sameOriginOrNone(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}
