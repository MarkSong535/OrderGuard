// Package assistant is the AI support assistant used by the interactive demo:
// a Go port of the canvas_ai agent (same tool-calling loop, prompt rules and
// default model), given OrderGuard's MCP tools.
//
// Each turn: the model must call a tool (tool_choice "required"); order tools
// run through OrderGuard's MCP endpoint with the signed-in customer's key; the
// turn ends when the model calls final_answer_tool, or, after MaxSteps, with
// an answer written from the observations gathered so far.
package assistant

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"
)

var (
	//go:embed prompt_system.tmpl
	systemPrompt string
	//go:embed prompt_task.tmpl
	taskPrompt string
	//go:embed prompt_final_pre.txt
	finalPre string
	//go:embed prompt_final_post.tmpl
	finalPost string

	systemTmpl    = template.Must(template.New("system").Parse(systemPrompt))
	taskTmpl      = template.Must(template.New("task").Parse(taskPrompt))
	finalPostTmpl = template.Must(template.New("final").Parse(finalPost))
)

const (
	finalAnswerTool = "final_answer_tool"
	maxObservation  = 16 << 10
	repeatNote      = "\n\n(You already called this tool with the same arguments for this question; this is the earlier result.)"
)

// Config configures the assistant.
type Config struct {
	Model           Model
	MCPURL          string // OrderGuard's MCP endpoint, e.g. http://127.0.0.1:8080/mcp
	DiscoveryKey    string // key used only for tools/list, so guests get the same tools
	MaxSteps        int
	MaxHistoryTurns int
	SessionTTL      time.Duration
	MaxSessions     int
	TurnTimeout     time.Duration
	HTTP            *http.Client // for MCP calls
	Log             *slog.Logger
}

// Call describes one order lookup the assistant made, for the demo page.
type Call struct {
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args"`
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	Results    *int           `json:"results,omitempty"`
	Source     string         `json:"source,omitempty"`
	DurationMS int64          `json:"duration_ms"`
}

// Reply is the outcome of one chat turn.
type Reply struct {
	Answer    string `json:"answer"`
	ToolCalls []Call `json:"tool_calls"`
}

// Assistant keeps one conversation per (browser session, demo user).
type Assistant struct {
	cfg Config

	mu       sync.Mutex
	sessions map[sessionKey]*session

	toolsMu sync.Mutex
	tools   []MCPTool
}

type sessionKey struct{ session, user string }

type session struct {
	mu       sync.Mutex
	mcp      *mcpClient
	turns    [][]Message // earlier exchanges, oldest first
	lastUsed time.Time
}

// New returns an assistant; zero config values get canvas_ai's defaults.
func New(cfg Config) (*Assistant, error) {
	if cfg.Model == nil || cfg.MCPURL == "" {
		return nil, errors.New("assistant: Model and MCPURL are required")
	}
	cfg.MaxSteps = cmp.Or(cfg.MaxSteps, 6)
	cfg.MaxHistoryTurns = cmp.Or(cfg.MaxHistoryTurns, 10)
	cfg.SessionTTL = cmp.Or(cfg.SessionTTL, 30*time.Minute)
	cfg.MaxSessions = cmp.Or(cfg.MaxSessions, 200)
	cfg.TurnTimeout = cmp.Or(cfg.TurnTimeout, 60*time.Second)
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Assistant{cfg: cfg, sessions: map[sessionKey]*session{}}, nil
}

// Chat runs one turn. apiKey is the chosen user's OrderGuard key ("" for a guest).
func (a *Assistant) Chat(ctx context.Context, sessionID, userID, apiKey, message string, reset bool) (*Reply, error) {
	s := a.session(sessionKey{sessionID, userID}, apiKey, reset)
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, a.cfg.TurnTimeout)
	defer cancel()
	return a.run(ctx, s, strings.TrimSpace(message))
}

func (a *Assistant) session(k sessionKey, apiKey string, reset bool) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if reset {
		delete(a.sessions, k)
	}
	if s, ok := a.sessions[k]; ok {
		s.lastUsed = now
		return s
	}
	for key, s := range a.sessions {
		if now.Sub(s.lastUsed) > a.cfg.SessionTTL {
			delete(a.sessions, key)
		}
	}
	for len(a.sessions) >= a.cfg.MaxSessions {
		var oldest sessionKey
		first := true
		for key, s := range a.sessions {
			if first || s.lastUsed.Before(a.sessions[oldest].lastUsed) {
				oldest, first = key, false
			}
		}
		delete(a.sessions, oldest)
	}
	s := &session{mcp: &mcpClient{url: a.cfg.MCPURL, apiKey: apiKey, http: a.cfg.HTTP}, lastUsed: now}
	a.sessions[k] = s
	return s
}

// toolSpecs discovers OrderGuard's tools once; the list is the same for every user.
func (a *Assistant) toolSpecs(ctx context.Context) ([]MCPTool, error) {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	if a.tools != nil {
		return a.tools, nil
	}
	c := &mcpClient{url: a.cfg.MCPURL, apiKey: a.cfg.DiscoveryKey, http: a.cfg.HTTP}
	tools, err := c.listTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover tools: %w", err)
	}
	a.tools = tools
	return tools, nil
}

func (a *Assistant) run(ctx context.Context, s *session, task string) (*Reply, error) {
	specs, err := a.toolSpecs(ctx)
	if err != nil {
		return nil, err
	}
	system, err := renderSystem(specs)
	if err != nil {
		return nil, err
	}
	tools := toolDefs(specs)

	var history []Message
	for _, t := range s.turns {
		history = append(history, t...)
	}
	var taskText bytes.Buffer
	if err := taskTmpl.Execute(&taskText, task); err != nil {
		return nil, err
	}
	turn := []Message{{Role: "user", Content: text(taskText.String())}}
	reply := &Reply{ToolCalls: []Call{}}
	seen := map[string]string{} // tool+arguments -> observation, for this question only

	for step := 1; step <= a.cfg.MaxSteps; step++ {
		msgs := slices.Concat([]Message{{Role: "system", Content: text(system)}}, history, turn)
		resp, err := a.cfg.Model.Complete(ctx, msgs, tools, "required")
		if err != nil {
			return nil, err
		}
		if answer, ok := finalAnswer(resp); ok {
			reply.Answer = answer
			a.remember(s, turn, answer)
			return reply, nil
		}
		if len(resp.ToolCalls) == 0 {
			turn = append(turn, Message{Role: "assistant", Content: text(resp.text())},
				Message{Role: "user", Content: text("Reply by calling a tool; use final_answer_tool to answer the customer.")})
			continue
		}
		turn = append(turn, Message{Role: "assistant", ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			out := a.execute(ctx, s, specs, tc, seen, reply)
			turn = append(turn, Message{Role: "tool", ToolCallID: tc.ID, Content: text(out)})
		}
	}

	// Out of steps: answer from what was gathered (canvas_ai's provide_final_answer).
	var post bytes.Buffer
	_ = finalPostTmpl.Execute(&post, task)
	msgs := slices.Concat([]Message{{Role: "system", Content: text(finalPre)}}, history, turn,
		[]Message{{Role: "user", Content: text(post.String())}})
	resp, err := a.cfg.Model.Complete(ctx, msgs, nil, "")
	if err != nil {
		return nil, err
	}
	reply.Answer = strings.TrimSpace(resp.text())
	a.remember(s, turn, reply.Answer)
	return reply, nil
}

// finalAnswer extracts the answer from a final_answer_tool call (or a plain reply, if the model gave one).
func finalAnswer(m Message) (string, bool) {
	for _, tc := range m.ToolCalls {
		if tc.Function.Name != finalAnswerTool {
			continue
		}
		var args struct {
			Answer string `json:"answer"`
		}
		if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil && strings.TrimSpace(args.Answer) != "" {
			return strings.TrimSpace(args.Answer), true
		}
	}
	if len(m.ToolCalls) == 0 && strings.TrimSpace(m.text()) != "" {
		return strings.TrimSpace(m.text()), true
	}
	return "", false
}

// remember stores the turn (ending with the answer as plain text) and keeps the last MaxHistoryTurns.
func (a *Assistant) remember(s *session, turn []Message, answer string) {
	s.turns = append(s.turns, append(turn, Message{Role: "assistant", Content: text(answer)}))
	if over := len(s.turns) - a.cfg.MaxHistoryTurns; over > 0 {
		s.turns = s.turns[over:]
	}
}

func (a *Assistant) execute(ctx context.Context, s *session, specs []MCPTool, tc ToolCall, seen map[string]string, reply *Reply) string {
	name := tc.Function.Name
	if !slices.ContainsFunc(specs, func(t MCPTool) bool { return t.Name == name }) {
		return "error: unknown tool " + name
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(cmp.Or(tc.Function.Arguments, "{}")), &raw); err != nil {
		return "error: the arguments were not valid JSON"
	}
	args := map[string]any{}
	for k, v := range raw {
		if v != nil { // models send null for optional inputs
			args[k] = v
		}
	}
	key, _ := json.Marshal([]any{name, args}) // encoding/json sorts map keys
	if prev, ok := seen[string(key)]; ok {
		return prev + repeatNote
	}

	start := time.Now()
	call := Call{Tool: name, Args: args}
	out := a.callOrderGuard(ctx, s, &call)
	call.DurationMS = time.Since(start).Milliseconds()
	reply.ToolCalls = append(reply.ToolCalls, call)
	if len(out) > maxObservation {
		out = out[:maxObservation] + "\n(truncated)"
	}
	seen[string(key)] = out
	return out
}

func (a *Assistant) callOrderGuard(ctx context.Context, s *session, call *Call) string {
	res, err := s.mcp.callTool(ctx, call.Tool, call.Args)
	switch {
	case errors.Is(err, errMCPUnauthorized):
		call.Error = "unauthorized"
		return "error: unauthorized: the customer is not signed in"
	case err != nil:
		a.cfg.Log.WarnContext(ctx, "assistant tool call failed", "tool", call.Tool, "error", err)
		call.Error = "unreachable"
		return "error: order_system_unreachable: order information is temporarily unavailable"
	}

	var texts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	out := strings.Join(texts, "\n")
	var sc struct {
		Results *[]json.RawMessage `json:"results"`
		Meta    struct {
			Source    string `json:"source"`
			FetchedAt string `json:"fetched_at"`
		} `json:"meta"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(res.StructuredContent, &sc)

	if res.IsError {
		call.Error = cmp.Or(sc.Error.Code, "error")
		return cmp.Or(out, "error: the lookup failed")
	}
	call.OK = true
	call.Source = sc.Meta.Source
	if sc.Results != nil {
		n := len(*sc.Results)
		call.Results = &n
	}
	if sc.Meta.Source == "stale" {
		out = fmt.Sprintf("STALE DATA: the order system is unavailable; this copy was last updated %s.\n%s", readableTime(sc.Meta.FetchedAt), out)
	}
	return out
}

// readableTime turns an RFC 3339 timestamp into wording the model can repeat
// to a customer as is ("on 23 Oct 2026 at 20:16 UTC").
func readableTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "recently"
	}
	return t.UTC().Format("on 2 Jan 2006 at 15:04 UTC")
}

type promptTool struct {
	Name, Description, Inputs string
}

func renderSystem(specs []MCPTool) (string, error) {
	tools := make([]promptTool, 0, len(specs)+1)
	for _, t := range specs {
		props, _ := json.Marshal(t.InputSchema["properties"])
		tools = append(tools, promptTool{t.Name, t.Description, string(props)})
	}
	tools = append(tools, promptTool{finalAnswerTool, "Gives your reply to the customer.", `{"answer":{"type":"string"}}`})
	var b bytes.Buffer
	err := systemTmpl.Execute(&b, tools)
	return b.String(), err
}

func toolDefs(specs []MCPTool) []Tool {
	defs := make([]Tool, 0, len(specs)+1)
	for _, t := range specs {
		params := t.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		defs = append(defs, Tool{Type: "function", Function: ToolFunction{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	return append(defs, Tool{Type: "function", Function: ToolFunction{
		Name:        finalAnswerTool,
		Description: "Provides your final reply to the customer.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string", "description": "The reply to the customer."}},
			"required":   []string{"answer"},
		},
	}})
}
