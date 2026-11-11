package bridge

import (
	"context"
	"encoding/json"
	"github.com/os7blue/feishu-codex-bridge/internal/modelsettings"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
	"time"
)

// Frontend delivery capabilities are optional and selected by the application.
// Every message has a stable delivery key. Routed delivery returns native IDs
// so replies can be tied back to the exact conversation.
type RoutedMessenger interface {
	SendRouted(context.Context, string, string, string) ([]string, error)
}
type RichMessenger interface {
	SendCardRouted(context.Context, string, json.RawMessage, string) ([]string, error)
}
type FileTransport interface {
	DownloadFile(context.Context, string, Attachment) ([]byte, error)
	SendFile(context.Context, string, string, []byte, string) ([]string, error)
}

// Repository is the application's session and routing port. File-backed state
// is wired by main; another frontend does not construct or edit saved state.
type Repository interface {
	modelsettings.Repository
	PairedID() string
	Pair(string) error
	Binding(string) (state.Binding, bool)
	Bind(string, state.Binding) error
	Unbind(string) error
	Claim(string, time.Time) (bool, error)
	RememberSession(state.Session) (state.Session, error)
	Session(string) (state.Session, bool)
	Sessions(string) []state.Session
	SessionByRef(int) (state.Session, bool)
	ClaimHandoff(string) (bool, error)
	RememberRoutes([]string, state.MessageRoute) error
	Route(string, string, string) (state.MessageRoute, bool)
	OwnerChat() (string, bool)
}
