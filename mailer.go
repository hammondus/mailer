// Package mailer composes and sends RFC 5322 email over SMTP using nothing but
// the Go standard library.
//
// The package is deliberately transport-generic. Amazon SES is just an SMTP
// host, so the same code works unchanged against SES, Postmark, Mailgun,
// Fastmail or a local Postfix/Mailpit. SESHost builds the SES endpoint name.
//
// # Typical use
//
//	cfg := mailer.ConfigFromEnv("MYAPP_SMTP_")
//
//	var sender mailer.Sender
//	if cfg.Configured() {
//		sender = mailer.NewSMTP(cfg)
//	} else {
//		sender = mailer.NewLog(nil) // dev fallback: log instead of sending
//	}
//
//	err := sender.Send(ctx, &mailer.Message{
//		To:      []string{"user@example.com"},
//		Subject: "Verify your API key",
//		Text:    "Open https://example.com/verify?t=abc123 to finish.",
//	})
//
// # Amazon SES notes
//
//   - Host is email-smtp.<region>.amazonaws.com; see SESHost.
//   - Username and Password are SES *SMTP credentials*, which are not IAM
//     access keys. Generate them in the SES console; the password is derived
//     from an IAM secret key by an HMAC and cannot be recovered afterwards.
//   - From must be a verified SES identity (an address or a whole domain).
//   - A new account sits in the SES sandbox: it may only send to verified
//     addresses, and is capped at 200 messages per 24 hours and 1 per second.
//     Set Config.RateLimit to stay under the per-second cap. Your account's
//     actual quotas are shown in the SES console — read them there rather than
//     discovering them by being throttled.
package mailer

import (
	"context"
	"crypto/tls"
	"os"
	"time"
)

// DefaultPort is the SMTP submission port, used when Config.Port is empty.
const DefaultPort = "587"

// DefaultTimeout bounds how long any single SMTP read or write may stall
// before the send is abandoned. See Config.Timeout.
const DefaultTimeout = 30 * time.Second

// Sender delivers a composed message. Both SMTPSender and the test/development
// helpers (LogSender, MemorySender) satisfy it, so an application can depend on
// the interface and swap the implementation by configuration.
type Sender interface {
	Send(ctx context.Context, m *Message) error
}

// Config holds SMTP connection settings and the default From address.
//
// Nothing here is read from the environment automatically; ConfigFromEnv is an
// opt-in helper. Keeping environment parsing at the call site means each
// application keeps its own variable-naming convention.
type Config struct {
	// Host is the SMTP server hostname, e.g. email-smtp.ap-southeast-2.amazonaws.com.
	Host string

	// Port defaults to DefaultPort ("587"). Ports 465 and 2465 use implicit
	// TLS; everything else negotiates STARTTLS.
	Port string

	// Username and Password authenticate to the server. Leave both empty to
	// skip authentication (a local relay that trusts the network, say).
	Username string
	Password string

	// From is the default envelope and header sender, used whenever a Message
	// leaves its own From empty. For SES it must be a verified identity.
	From string

	// Timeout is an *idle* timeout: the deadline is refreshed before every
	// read and write, so it bounds how long the conversation may stall rather
	// than how long it may take in total. That distinction matters for
	// SendMany, where a legitimate batch can run for minutes.
	//
	// Zero means DefaultTimeout.
	Timeout time.Duration

	// RateLimit caps outbound messages per second; zero means no limit. It
	// paces sends evenly rather than allowing a burst, because SES throttles
	// on rate and a burst simply buys a 454 error. Fractional values are fine:
	// 0.2 is one message every five seconds.
	RateLimit float64

	// LocalName is the hostname announced in the EHLO greeting. Zero means
	// "localhost", which SES accepts.
	LocalName string

	// AllowInsecure permits sending over a connection that was never upgraded
	// to TLS. It is off by default: without it, a server that does not offer
	// STARTTLS is an error rather than a silent downgrade that would put the
	// password on the wire in clear text. Turn it on only for a local test
	// relay such as Mailpit.
	AllowInsecure bool

	// TLSConfig overrides the TLS settings. ServerName is filled in from Host
	// when empty. Leave nil for the sane default.
	TLSConfig *tls.Config
}

// Configured reports whether enough is set to send real email. When it is
// false the caller should fall back to NewLog so local development still
// exercises the surrounding code path.
func (c Config) Configured() bool {
	return c.Host != "" && c.From != ""
}

// withDefaults returns a copy with the zero-valued fields filled in.
func (c Config) withDefaults() Config {
	if c.Port == "" {
		c.Port = DefaultPort
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.LocalName == "" {
		c.LocalName = "localhost"
	}
	return c
}

// ConfigFromEnv fills the connection fields from environment variables sharing
// a prefix: <prefix>HOST, <prefix>PORT, <prefix>USERNAME, <prefix>PASSWORD and
// <prefix>FROM. A typical call is ConfigFromEnv("MYAPP_SMTP_").
//
// The remaining fields (Timeout, RateLimit, AllowInsecure, TLSConfig) are
// deliberately not read from the environment: they are deployment decisions
// that belong in code, and AllowInsecure in particular should never be a
// production environment variable away from disabling TLS.
func ConfigFromEnv(prefix string) Config {
	return Config{
		Host:     os.Getenv(prefix + "HOST"),
		Port:     os.Getenv(prefix + "PORT"),
		Username: os.Getenv(prefix + "USERNAME"),
		Password: os.Getenv(prefix + "PASSWORD"),
		From:     os.Getenv(prefix + "FROM"),
	}
}

// SESHost returns the Amazon SES SMTP endpoint for a region, e.g.
// SESHost("ap-southeast-2") is "email-smtp.ap-southeast-2.amazonaws.com".
func SESHost(region string) string {
	return "email-smtp." + region + ".amazonaws.com"
}
