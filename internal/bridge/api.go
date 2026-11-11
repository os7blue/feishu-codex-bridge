package bridge

import (
	"errors"
	"strings"

	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
)

// Command is the structured entry for a frontend's buttons or API. The adapter
// supplies authenticated identity and a stable request ID. Natural-language
// messages use Submit; interactive replies use HandleAction. All three entries
// reach the same durable inbox and authorization checks.
type Command struct {
	ChatID, UserID, RequestID string
	Name, Argument            string
	ThreadID                  string
}

func (b *Bridge) SubmitCommand(command Command) error {
	if !b.authorized(command.UserID) || command.ChatID == "" || command.RequestID == "" {
		return errors.New("authenticated command identity required")
	}
	known := false
	for _, entry := range commandset.All() {
		if command.Name == entry.Name {
			known = true
			break
		}
	}
	if !known || strings.ContainsAny(command.Name, "/ \t\r\n") {
		return errors.New("unknown application command")
	}
	if command.ThreadID != "" {
		session, ok := b.store.Session(command.ThreadID)
		if !ok || session.OwnerID != command.UserID {
			return errors.New("command session ownership required")
		}
	}
	text := "/" + command.Name
	if command.Argument != "" {
		text += " " + command.Argument
	}
	return b.Submit(Input{ChatID: command.ChatID, UserID: command.UserID, MessageID: command.RequestID, Text: text, ThreadID: command.ThreadID, Explicit: command.ThreadID != ""})
}
