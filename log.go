package mailer

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// LogSender logs messages instead of sending them. Use it when Config is not
// Configured, so local development still runs the whole surrounding code path
// — and so the verification link or reset token you need is visible in the
// logs rather than lost.
type LogSender struct {
	log *slog.Logger
}

// NewLog returns a Sender that logs. A nil logger means slog.Default.
//
// It takes a logger rather than reaching for the global log package, because a
// library that writes to someone else's global logger is a nuisance to host.
func NewLog(l *slog.Logger) *LogSender {
	if l == nil {
		l = slog.Default()
	}
	return &LogSender{log: l}
}

// Send logs a summary at info level and the fully composed message at debug
// level, then reports success. Composition still runs, so a malformed message
// fails in development exactly as it would in production.
func (s *LogSender) Send(ctx context.Context, m *Message) error {
	raw, err := m.Build("dev@localhost")
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "mailer: SMTP not configured, message not sent",
		"to", strings.Join(m.To, ", "),
		"subject", m.Subject,
		"bytes", len(raw),
	)
	s.log.DebugContext(ctx, "mailer: composed message", "raw", string(raw))
	return nil
}

// MemorySender records messages instead of sending them, for use in the tests
// of a consuming application. It is safe for concurrent use.
type MemorySender struct {
	// Err, when set, is returned by every Send and the message is not
	// recorded. Use it to exercise the caller's error handling.
	Err error

	mu   sync.Mutex
	sent []*Message
}

// Send records the message after checking that it composes.
func (s *MemorySender) Send(ctx context.Context, m *Message) error {
	if s.Err != nil {
		return s.Err
	}
	// Compose and discard: a test that never builds the message would not
	// catch a caller assembling an invalid one.
	if _, err := m.Build("test@localhost"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

// Sent returns a copy of the recorded messages, oldest first.
func (s *MemorySender) Sent() []*Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Message(nil), s.sent...)
}

// Reset discards the recorded messages.
func (s *MemorySender) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = nil
}
