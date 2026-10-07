package mailer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestConfigured(t *testing.T) {
	cases := []struct {
		cfg  Config
		want bool
	}{
		{Config{}, false},
		{Config{Host: "smtp.example.com"}, false},                       // no From
		{Config{From: "a@example.com"}, false},                          // no Host
		{Config{Host: "smtp.example.com", From: "a@example.com"}, true}, // minimal
	}
	for _, c := range cases {
		if got := c.cfg.Configured(); got != c.want {
			t.Errorf("Configured(%+v) = %v, want %v", c.cfg, got, c.want)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{Host: "smtp.example.com", From: "a@example.com"}.withDefaults()
	if got.Port != DefaultPort {
		t.Errorf("Port = %q, want %q", got.Port, DefaultPort)
	}
	if got.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, DefaultTimeout)
	}
	if got.LocalName != "localhost" {
		t.Errorf("LocalName = %q, want localhost", got.LocalName)
	}

	// Explicit values must survive.
	set := Config{Port: "2587", Timeout: time.Second, LocalName: "mx.example.com"}.withDefaults()
	if set.Port != "2587" || set.Timeout != time.Second || set.LocalName != "mx.example.com" {
		t.Errorf("withDefaults overwrote explicit values: %+v", set)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("TESTAPP_SMTP_HOST", "smtp.example.com")
	t.Setenv("TESTAPP_SMTP_PORT", "2587")
	t.Setenv("TESTAPP_SMTP_USERNAME", "user")
	t.Setenv("TESTAPP_SMTP_PASSWORD", "secret")
	t.Setenv("TESTAPP_SMTP_FROM", "noreply@example.com")

	got := ConfigFromEnv("TESTAPP_SMTP_")
	want := Config{
		Host: "smtp.example.com", Port: "2587",
		Username: "user", Password: "secret", From: "noreply@example.com",
	}
	if got != want {
		t.Errorf("ConfigFromEnv = %+v, want %+v", got, want)
	}
	if !got.Configured() {
		t.Error("Configured() = false for a fully populated environment")
	}

	// An unset prefix must yield a config that reports itself unconfigured,
	// which is what drives the LogSender fallback in development.
	if ConfigFromEnv("NOTHING_SET_").Configured() {
		t.Error("Configured() = true with no environment set")
	}
}

func TestSESHost(t *testing.T) {
	if got, want := SESHost("ap-southeast-2"), "email-smtp.ap-southeast-2.amazonaws.com"; got != want {
		t.Errorf("SESHost = %q, want %q", got, want)
	}
}

func TestLogSenderComposesButDoesNotSend(t *testing.T) {
	s := NewLog(nil)
	if err := s.Send(t.Context(), &Message{
		To: []string{"a@example.com"}, Subject: "hi", Text: "body",
	}); err != nil {
		t.Errorf("Send: %v", err)
	}
	// A message that would fail in production must fail here too, or local
	// development would hide the bug until deploy.
	if err := s.Send(t.Context(), &Message{To: []string{"a@example.com"}}); err == nil {
		t.Error("LogSender accepted a message with no body")
	}
}

func TestLogSenderLogsReadableText(t *testing.T) {
	var buf bytes.Buffer
	s := NewLog(slog.New(slog.NewTextHandler(&buf, nil))) // info level
	link := "https://example.com/verify?token=abc123"
	if err := s.Send(t.Context(), &Message{
		To: []string{"a@example.com"}, Subject: "Verify", Text: "Open " + link + " to finish.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The link must survive verbatim at the default level: the composed body
	// is quoted-printable, where it would read token=3Dabc123.
	if !strings.Contains(buf.String(), link) {
		t.Errorf("info log does not contain the link verbatim:\n%s", buf.String())
	}
}

func TestMemorySender(t *testing.T) {
	var s MemorySender
	var _ Sender = &s // it must satisfy the interface consumers depend on

	m := &Message{To: []string{"a@example.com"}, Subject: "hi", Text: "body"}
	if err := s.Send(t.Context(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := s.Sent()
	if len(sent) != 1 || sent[0].Subject != "hi" {
		t.Fatalf("Sent() = %+v, want one message with subject hi", sent)
	}

	// Sent returns a copy, so a caller cannot corrupt the record.
	sent[0] = nil
	if s.Sent()[0] == nil {
		t.Error("Sent() exposed the underlying slice")
	}

	s.Reset()
	if len(s.Sent()) != 0 {
		t.Error("Reset did not clear the record")
	}

	want := errors.New("boom")
	s.Err = want
	if err := s.Send(t.Context(), m); !errors.Is(err, want) {
		t.Errorf("Send with Err set = %v, want %v", err, want)
	}
	if len(s.Sent()) != 0 {
		t.Error("a failed Send was still recorded")
	}
}

func TestSendersSatisfyInterface(t *testing.T) {
	var _ Sender = (*SMTPSender)(nil)
	var _ Sender = (*LogSender)(nil)
	var _ Sender = (*MemorySender)(nil)
	_ = context.Background
}
