package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is a Chat Completions message.
type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a function the model may call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func text(s string) *string { return &s }

func (m Message) text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// Model completes a conversation. *OpenAI implements it; tests use fakes.
type Model interface {
	Complete(ctx context.Context, messages []Message, tools []Tool, toolChoice string) (Message, error)
}

// OpenAI is a minimal Chat Completions client.
type OpenAI struct {
	BaseURL         string // e.g. https://api.openai.com/v1
	APIKey          string
	Model           string
	ReasoningEffort string // "" omits the parameter
	HTTP            *http.Client
}

type chatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools,omitempty"`
	ToolChoice      string    `json:"tool_choice,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete implements Model.
func (c *OpenAI) Complete(ctx context.Context, messages []Message, tools []Tool, toolChoice string) (Message, error) {
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: messages, Tools: tools, ToolChoice: toolChoice, ReasoningEffort: c.ReasoningEffort})
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("model request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Message{}, fmt.Errorf("model response: %w", err)
	}
	var cr chatResponse
	_ = json.Unmarshal(raw, &cr)
	if resp.StatusCode != http.StatusOK {
		// The provider's message can contain account details; keep only the type for logs.
		kind := ""
		if cr.Error != nil {
			kind = cr.Error.Type
		}
		return Message{}, fmt.Errorf("model returned HTTP %d %s", resp.StatusCode, kind)
	}
	if len(cr.Choices) == 0 {
		return Message{}, errors.New("model returned no choices")
	}
	return cr.Choices[0].Message, nil
}
