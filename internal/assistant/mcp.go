package assistant

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"orderguard/internal/observability"
)

// The assistant reaches OrderGuard only through its MCP endpoint, with the
// signed-in customer's API key, so OrderGuard (not the prompt) decides what
// the model can see.

const mcpProtocolVersion = "2025-06-18"

var (
	errMCPUnauthorized = errors.New("mcp: unauthorized")
	errMCPUnreachable  = errors.New("mcp: unreachable")
)

// MCPTool is a tool advertised by tools/list.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpClient struct {
	url         string
	apiKey      string
	http        *http.Client
	ids         atomic.Int64
	initialized bool
	version     string
	sessionID   string
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *mcpClient) post(ctx context.Context, msg map[string]any) (*rpcReply, error) {
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.initialized {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if id := observability.RequestID(ctx); id != "" {
		req.Header.Set("X-Request-ID", id) // correlate tool calls with the chat request in logs
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMCPUnreachable, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errMCPUnauthorized
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d", errMCPUnreachable, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMCPUnreachable, err)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw = lastSSEData(raw)
	}
	var reply rpcReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("%w: invalid reply", errMCPUnreachable)
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	return &reply, nil
}

func lastSSEData(raw []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			last = bytes.TrimSpace(d)
		}
	}
	return last
}

func (c *mcpClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "id": c.ids.Add(1), "method": method}
	if params != nil {
		msg["params"] = params
	}
	reply, err := c.post(ctx, msg)
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return nil, fmt.Errorf("%w: empty reply", errMCPUnreachable)
	}
	return reply.Result, nil
}

func (c *mcpClient) initialize(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	res, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "orderguard-support-assistant", "version": "1.0.0"},
	})
	if err != nil {
		return err
	}
	var out struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(res, &out)
	c.version = cmp.Or(out.ProtocolVersion, mcpProtocolVersion)
	c.initialized = true
	_, err = c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return err
}

func (c *mcpClient) listTools(ctx context.Context) ([]MCPTool, error) {
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	res, err := c.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// toolResult is a tools/call result.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (*toolResult, error) {
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	res, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var out toolResult
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("%w: invalid tool result", errMCPUnreachable)
	}
	return &out, nil
}
