// Package presentation contains semantic UI contracts shared by frontends.
// Native JSON, SDK types, styling and transport code belong to adapters.
package presentation

import "encoding/json"

type Choice struct{ ID, Label string }
type View struct {
	Kind                                string // panel, menu, draft, submitted, request, demo
	Title, Context, Token, Target, Text string
	Request                             bool
	Choices                             []Choice
}
type Renderer interface {
	Format() string
	Render(View) json.RawMessage
}

// Text is an intentional capability fallback for a frontend without rich UI.
type Text struct{}

func (Text) Format() string              { return "text" }
func (Text) Render(View) json.RawMessage { return nil }

// Action carries an authenticated frontend callback after native decoding.
// Tokens are opaque; the application rechecks owner, target and expiry.
type Action struct{ ChatID, UserID, MessageID, Token, Action, Command, Text string }
type Result struct {
	Message string
	Error   bool
	Card    json.RawMessage
}
