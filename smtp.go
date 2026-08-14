package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"time"
)

// SMTPSender delivers messages over SMTP. It is safe for concurrent use.
type SMTPSender struct {
	cfg     Config
	limiter *limiter
}

// NewSMTP builds a sender from cfg, applying defaults for the zero fields.
func NewSMTP(cfg Config) *SMTPSender {
	cfg = cfg.withDefaults()
	return &SMTPSender{cfg: cfg, limiter: newLimiter(cfg.RateLimit)}
}

// Send delivers one message, opening and closing a connection around it.
func (s *SMTPSender) Send(ctx context.Context, m *Message) error {
	errs := s.SendMany(ctx, []*Message{m})
	return errs[0]
}

// SendMany delivers a batch over a single connection, which avoids paying for
// a TCP and TLS handshake per message.
//
// The returned slice is parallel to msgs, holding nil where the message was
// accepted. A per-message failure (a rejected recipient, say) does not abandon
// the batch; a connection-level failure does, and is recorded against every
// message that had not yet been sent. Collapse the slice with errors.Join if
// you do not care which message failed.
func (s *SMTPSender) SendMany(ctx context.Context, msgs []*Message) []error {
	errs := make([]error, len(msgs))
	if len(msgs) == 0 {
		return errs
	}

	c, cleanup, err := s.connect(ctx)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	defer cleanup()

	for i, m := range msgs {
		if err := s.limiter.wait(ctx); err != nil {
			// Cancellation is a batch-level condition: everything from here on
			// is equally undelivered.
			for j := i; j < len(errs); j++ {
				errs[j] = err
			}
			return errs
		}
		if err := s.deliver(c, m); err != nil {
			errs[i] = err
			// A refused recipient leaves the session usable, but a broken
			// connection does not. Reset tells us which we have; if it fails,
			// the remaining messages cannot be sent on this connection.
			if rerr := c.Reset(); rerr != nil {
				for j := i + 1; j < len(errs); j++ {
					errs[j] = fmt.Errorf("mailer: connection unusable after an earlier failure: %w", rerr)
				}
				return errs
			}
		}
	}
	if err := c.Quit(); err != nil {
		// The messages were already accepted at this point, so a failed QUIT
		// is not worth reporting as a delivery failure.
		_ = err
	}
	return errs
}

// deliver runs one MAIL/RCPT/DATA exchange on an established session.
func (s *SMTPSender) deliver(c *smtp.Client, m *Message) error {
	from := m.From
	if from == "" {
		from = s.cfg.From
	}
	raw, err := m.Build(s.cfg.From)
	if err != nil {
		return err
	}
	rcpts, err := m.Recipients()
	if err != nil {
		return err
	}
	// The envelope sender must be a bare address, with no display name.
	envelopeFrom, err := bareAddress(from)
	if err != nil {
		return err
	}

	if err := c.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("mailer: MAIL FROM %s: %w", envelopeFrom, err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("mailer: RCPT TO %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	// The writer returned by Data is a dot-writer: it escapes a leading "."
	// and normalises line endings, so the body needs no further escaping.
	if _, err := w.Write(raw); err != nil {
		w.Close()
		return fmt.Errorf("mailer: write message body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: complete message: %w", err)
	}
	return nil
}

// connect dials, upgrades to TLS and authenticates, returning a live session
// and a cleanup function that must be called.
func (s *SMTPSender) connect(ctx context.Context) (*smtp.Client, func(), error) {
	cfg := s.cfg
	addr := net.JoinHostPort(cfg.Host, cfg.Port)

	tlsCfg := cfg.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: cfg.Host}
	} else if tlsCfg.ServerName == "" {
		tlsCfg = tlsCfg.Clone()
		tlsCfg.ServerName = cfg.Host
	}

	dialer := &net.Dialer{Timeout: cfg.Timeout}
	implicitTLS := cfg.Port == "465" || cfg.Port == "2465"

	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("mailer: dial %s: %w", addr, err)
	}
	conn = &idleConn{Conn: conn, timeout: cfg.Timeout}

	// net/smtp predates context, so cancellation is delivered by closing the
	// connection underneath it, which surfaces as a read/write error.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	cleanup := func() {
		close(done)
		conn.Close()
	}
	fail := func(err error) (*smtp.Client, func(), error) {
		cleanup()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("mailer: %w", ctxErr)
		}
		return nil, nil, err
	}

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fail(fmt.Errorf("mailer: SMTP greeting from %s: %w", addr, err))
	}
	if err := c.Hello(cfg.LocalName); err != nil {
		return fail(fmt.Errorf("mailer: EHLO %s: %w", cfg.LocalName, err))
	}

	secure := implicitTLS
	if !secure {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fail(fmt.Errorf("mailer: STARTTLS: %w", err))
			}
			secure = true
		}
	}
	if !secure && !cfg.AllowInsecure {
		return fail(fmt.Errorf("mailer: %s does not offer STARTTLS; refusing to send in the clear (set Config.AllowInsecure to override)", addr))
	}

	if cfg.Username != "" {
		// PlainAuth refuses to hand credentials to an unencrypted, non-local
		// server on its own account, which is the behaviour we want.
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fail(fmt.Errorf("mailer: authenticate as %s: %w", cfg.Username, err))
		}
	}
	return c, cleanup, nil
}

// bareAddress strips any display name, returning just the addr-spec.
func bareAddress(s string) (string, error) {
	if s == "" {
		return "", errors.New("mailer: no From address configured")
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", fmt.Errorf("mailer: From address %q: %w", s, err)
	}
	return a.Address, nil
}

// idleConn refreshes the read/write deadline before every operation, turning
// the absolute deadline net.Conn offers into an idle timeout.
//
// This is the fix for the failure mode a plain dial timeout does not cover: a
// server that completes the TCP handshake and then stops responding mid-DATA
// would otherwise hang the caller indefinitely. An idle timeout, rather than
// one absolute deadline, is what lets SendMany run a long batch without
// arbitrarily capping the whole conversation.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
