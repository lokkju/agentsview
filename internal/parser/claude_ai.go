package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

type claudeAIConversation struct {
	UUID        string            `json:"uuid"`
	Name        string            `json:"name"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Messages    []claudeAIMessage `json:"chat_messages"`
	CurrentLeaf jsontext.Value    `json:"current_leaf_message_uuid"`
}

type claudeAIMessage struct {
	UUID        string               `json:"uuid"`
	Parent      jsontext.Value       `json:"parent_message_uuid"`
	Text        string               `json:"text"`
	Content     []claudeAIBlock      `json:"content"`
	Sender      string               `json:"sender"`
	CreatedAt   string               `json:"created_at"`
	Attachments []claudeAIAttachment `json:"attachments"`
	// Files and FilesV2 stay raw so an entry of an unexpected shape is
	// skipped instead of failing the whole conversation.
	Files   []jsontext.Value `json:"files"`
	FilesV2 []jsontext.Value `json:"files_v2"`
}

// claudeAIBlock represents a content block within a message.
// Block types: text, thinking, tool_use, tool_result,
// voice_note, token_budget.
type claudeAIBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	// tool holds the raw tool_use or tool_result block. Tool fields are
	// decoded leniently from it, so a tool block of an unexpected shape
	// loses its call instead of failing the conversation.
	tool jsontext.Value
}

// UnmarshalJSON decodes the display fields strictly, as before tool blocks
// were read, and keeps tool blocks raw for lenient decoding.
func (b *claudeAIBlock) UnmarshalJSON(data []byte) error {
	var base struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	*b = claudeAIBlock{Type: base.Type, Text: base.Text, Thinking: base.Thinking}
	if base.Type == "tool_use" || base.Type == "tool_result" {
		b.tool = jsontext.Value(slices.Clone(data))
	}
	return nil
}

// ClaudeAIExportParser is implemented by the Claude.ai import-only provider to
// stream a Claude.ai conversations export. Claude.ai sessions are never
// discovered or synced from disk; they only enter the archive through a
// one-shot import, so this entry point lives on the provider rather than the
// Discover/Parse path. Callers obtain it via NewProvider(AgentClaudeAI, ...)
// and a type assertion.
type ClaudeAIExportParser interface {
	// ParseClaudeAIExport streams a Claude.ai conversations.json export and
	// calls onConversation for each non-empty conversation.
	ParseClaudeAIExport(
		r io.Reader,
		onConversation func(ParseResult) error,
	) error
}

// claudeAIAttachment holds content emitted by Claude attachments.
type claudeAIAttachment struct {
	FileName         string `json:"file_name"`
	ExtractedContent string `json:"extracted_content"`
}

// ParseClaudeAIExport streams a Claude.ai conversations.json
// export and calls onConversation for each non-empty
// conversation.
func (p *claudeAIImportOnlyProvider) ParseClaudeAIExport(
	r io.Reader,
	onConversation func(ParseResult) error,
) error {
	dec := jsontext.NewDecoder(r)

	tok, err := dec.ReadToken()
	if err != nil {
		return fmt.Errorf("reading opening token: %w", err)
	}
	if tok.Kind() != jsontext.KindBeginArray {
		return fmt.Errorf("expected JSON array, got %v", tok)
	}

	for dec.PeekKind() != jsontext.KindEndArray {
		var conv claudeAIConversation
		if err := json.UnmarshalDecode(dec, &conv); err != nil {
			return fmt.Errorf("decoding conversation: %w", err)
		}

		if len(conv.Messages) == 0 {
			continue
		}

		result, err := convertClaudeAIConversation(conv)
		if err != nil {
			return fmt.Errorf(
				"converting conversation %s: %w",
				conv.UUID, err,
			)
		}

		if err := onConversation(result); err != nil {
			return err
		}
	}
	_, err = dec.ReadToken()
	return err
}

// ParseClaudeAIDetail selects the visible branch of a browser-fetched chat.
func ParseClaudeAIDetail(data []byte) (ParseResult, error) {
	var conv *claudeAIConversation
	if err := json.Unmarshal(data, &conv); err != nil {
		return ParseResult{}, err
	}
	if conv == nil || conv.UUID == "" || len(conv.Messages) == 0 {
		return ParseResult{}, errors.New("expected conversation with chat_messages")
	}
	messages, err := selectedClaudeAIPath(*conv)
	if err != nil {
		return ParseResult{}, err
	}
	conv.Messages = messages
	return convertClaudeAIConversation(*conv)
}

// assembleClaudeAIContent builds message content from content
// blocks. Falls back to the top-level text field when no
// content blocks have usable text.
func assembleClaudeAIContent(
	m claudeAIMessage,
) (content string, hasThinking bool) {
	attachmentParts := buildClaudeAttachmentText(m.Attachments)

	if len(m.Content) == 0 {
		if len(attachmentParts) == 0 {
			return m.Text, false
		}

		contentParts := make([]string, 0, 1+len(attachmentParts))
		if m.Text != "" {
			contentParts = append(contentParts, m.Text)
		}
		contentParts = append(contentParts, attachmentParts...)
		return strings.Join(contentParts, "\n\n"), false
	}

	var contentParts []string
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				contentParts = append(contentParts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				hasThinking = true
				contentParts = append(contentParts,
					"[Thinking]\n"+b.Thinking+"\n[/Thinking]")
			}
			// tool_use and tool_result become tool calls
			// (attachClaudeAITools) and stay out of the text so
			// archived rows keep their identity. voice_note and
			// token_budget are metadata blocks.
		}
	}

	if len(contentParts) == 0 {
		if len(attachmentParts) == 0 {
			return m.Text, hasThinking
		}
		if m.Text != "" {
			contentParts = append(contentParts, m.Text)
		}
	}

	contentParts = append(contentParts, attachmentParts...)

	return strings.Join(contentParts, "\n\n"), hasThinking
}

func buildClaudeAttachmentText(
	attachments []claudeAIAttachment,
) []string {
	parts := make([]string, 0, len(attachments))
	for _, a := range attachments {
		if strings.TrimSpace(a.ExtractedContent) == "" {
			continue
		}
		if a.FileName == "" {
			parts = append(parts, a.ExtractedContent)
			continue
		}
		parts = append(parts, "[Attachment: "+a.FileName+"]\n"+a.ExtractedContent)
	}
	return parts
}

// selectedClaudeAIPath walks the server leaf's ancestors back to the root.
func selectedClaudeAIPath(conv claudeAIConversation) ([]claudeAIMessage, error) {
	const root = "00000000-0000-4000-8000-000000000000"
	var leaf string
	if json.Unmarshal(conv.CurrentLeaf, &leaf) != nil || leaf == "" || leaf == root {
		return nil, errors.New("expected current_leaf_message_uuid string naming a message")
	}
	byID := make(map[string]claudeAIMessage, len(conv.Messages))
	for _, m := range conv.Messages {
		if _, exists := byID[m.UUID]; exists {
			return nil, fmt.Errorf("duplicate message uuid %s", m.UUID)
		}
		byID[m.UUID] = m
	}
	path := []claudeAIMessage{}
	seen := make(map[string]bool)
	for id := leaf; id != root; {
		m, exists := byID[id]
		if !exists {
			return nil, fmt.Errorf("message %s is missing", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("cycle at message %s", id)
		}
		seen[id] = true
		var parent string
		if m.Parent.Kind() != jsontext.KindString || json.Unmarshal(m.Parent, &parent) != nil {
			return nil, fmt.Errorf("message %s's parent must be a string", m.UUID)
		}
		path = append(path, m)
		id = parent
	}
	slices.Reverse(path)
	return path, nil
}

func convertClaudeAIConversation(
	conv claudeAIConversation,
) (ParseResult, error) {
	startedAt, err := time.Parse(time.RFC3339Nano, conv.CreatedAt)
	if err != nil {
		return ParseResult{},
			fmt.Errorf("parsing created_at: %w", err)
	}

	endedAt, err := time.Parse(time.RFC3339Nano, conv.UpdatedAt)
	if err != nil {
		return ParseResult{},
			fmt.Errorf("parsing updated_at: %w", err)
	}

	var (
		msgs             []ParsedMessage
		userCount        int
		firstUserMessage string
		calls            = map[string]claudeAICallRef{}
	)

	for i, m := range conv.Messages {
		content, hasThinking := assembleClaudeAIContent(m)

		role := RoleAssistant
		if m.Sender == "human" {
			role = RoleUser
			userCount++
			if firstUserMessage == "" {
				firstUserMessage = content
			}
		}

		ts, _ := time.Parse(time.RFC3339Nano, m.CreatedAt)

		msgs = append(msgs, ParsedMessage{
			Ordinal:       i,
			Role:          role,
			Content:       content,
			Timestamp:     ts,
			HasThinking:   hasThinking,
			ContentLength: len(content),
		})
		attachClaudeAITools(msgs, calls, m.Content)
	}

	return ParseResult{
		Session: ParsedSession{
			ID:               "claude-ai:" + conv.UUID,
			Project:          "claude.ai",
			Machine:          "local",
			Agent:            AgentClaudeAI,
			FirstMessage:     firstUserMessage,
			SessionName:      conv.Name,
			StartedAt:        startedAt,
			EndedAt:          endedAt,
			MessageCount:     len(conv.Messages),
			UserMessageCount: userCount,
		},
		Messages: msgs,
	}, nil
}

// claudeAICallRef locates an emitted tool call by message and call index.
type claudeAICallRef struct{ message, call int }

type claudeAIToolUse struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input jsontext.Value `json:"input"`
}

type claudeAIToolResult struct {
	ToolUseID string         `json:"tool_use_id"`
	Name      string         `json:"name"`
	Content   jsontext.Value `json:"content"`
	IsError   bool           `json:"is_error"`
}

// attachClaudeAITools turns the tool_use and tool_result blocks of the last
// message in msgs into tool calls on that message. The message's text and
// ordinal stay as they are, so archived rows keep their identity. A result
// pairs with its call by tool_use_id, searching earlier messages of the same
// branch; a named result without a known call becomes a call of its own.
func attachClaudeAITools(
	msgs []ParsedMessage, calls map[string]claudeAICallRef, blocks []claudeAIBlock,
) {
	idx := len(msgs) - 1
	msg := &msgs[idx]
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			var use claudeAIToolUse
			if json.Unmarshal(b.tool, &use) != nil || use.Name == "" {
				continue
			}
			// Compact the input so an export and a browser fetch of the
			// same chat store identical calls.
			input := ""
			if len(use.Input) > 0 && use.Input.Kind() != jsontext.KindNull &&
				use.Input.Compact() == nil {
				input = string(use.Input)
			}
			msg.ToolCalls = append(msg.ToolCalls, ParsedToolCall{
				ToolUseID: use.ID,
				ToolName:  use.Name,
				Category:  NormalizeToolCategory(use.Name),
				InputJSON: input,
			})
			msg.HasToolUse = true
			if use.ID != "" {
				calls[use.ID] = claudeAICallRef{idx, len(msg.ToolCalls) - 1}
			}
		case "tool_result":
			var result claudeAIToolResult
			if json.Unmarshal(b.tool, &result) != nil {
				continue
			}
			event := ParsedToolResultEvent{
				ToolUseID: result.ToolUseID,
				Content:   claudeAIToolResultText(result.Content),
			}
			if result.IsError {
				event.Status = "error"
			}
			if ref, ok := calls[result.ToolUseID]; ok && result.ToolUseID != "" {
				call := &msgs[ref.message].ToolCalls[ref.call]
				call.ResultEvents = append(call.ResultEvents, event)
				continue
			}
			if result.Name == "" {
				continue
			}
			msg.ToolCalls = append(msg.ToolCalls, ParsedToolCall{
				ToolUseID:    result.ToolUseID,
				ToolName:     result.Name,
				Category:     NormalizeToolCategory(result.Name),
				ResultEvents: []ParsedToolResultEvent{event},
			})
			msg.HasToolUse = true
		}
	}
}

// claudeAIToolResultText flattens a tool_result's content into readable,
// searchable text. Text blocks keep their text; web search results
// (knowledge blocks) keep their title, URL and excerpt. String content is
// used as is.
func claudeAIToolResultText(raw jsontext.Value) string {
	var text string
	if raw.Kind() == jsontext.KindString && json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []jsontext.Value
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		var block struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		}
		if json.Unmarshal(rawBlock, &block) != nil {
			continue
		}
		var fields []string
		for _, field := range []string{block.Title, block.URL, block.Text} {
			if strings.TrimSpace(field) != "" {
				fields = append(fields, field)
			}
		}
		if len(fields) > 0 {
			parts = append(parts, strings.Join(fields, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}
