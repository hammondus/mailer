# mailer

Compose and send email over SMTP using nothing but the Go standard library.

Zero dependencies. Built for Amazon SES, but SES is only a hostname — the same
code works unchanged against Postmark, Mailgun, Fastmail or a local Postfix.

```
go get github.com/hammondus/mailer
```

## Why

Extracted from a `internal/mailer` package that had been copied between
projects. The copies each carried the same defects: no deadline once the
connection was established (so a stalled server could hang a request handler
forever), a silent fallback to plaintext when a server did not advertise
STARTTLS, no RFC 2047 encoding on the subject, no `context` support, and no
HTML or attachments. See [DESIGN-DECISIONS.md](DESIGN-DECISIONS.md).

## Usage

```go
cfg := mailer.ConfigFromEnv("MYAPP_SMTP_") // HOST, PORT, USERNAME, PASSWORD, FROM

// Fall back to logging when SMTP is not configured, so local development
// still runs the whole path and the verification link is visible.
var sender mailer.Sender
if cfg.Configured() {
    sender = mailer.NewSMTP(cfg)
} else {
    sender = mailer.NewLog(nil)
}

err := sender.Send(ctx, &mailer.Message{
    To:      []string{"user@example.com"},
    Subject: "Verify your API key",
    Text:    "Open https://example.com/verify?t=abc123 to finish.",
})
```

`Sender` is a one-method interface, so an application depends on the interface
and picks an implementation by configuration.

### HTML with a plain-text alternative

Supply both bodies. The result is a `multipart/alternative`, and sending a
plain-text alternative alongside HTML measurably helps deliverability.

```go
err := sender.Send(ctx, &mailer.Message{
    To:      []string{"user@example.com"},
    Subject: "Your receipt",
    Text:    "Thanks. Your receipt is attached.",
    HTML:    "<p>Thanks. Your receipt is attached.</p>",
})
```

### Attachments and inline images

An `Attachment` with a `ContentID` becomes an inline resource referenced from
the HTML as `cid:`; without one it is a regular attachment.

```go
err := sender.Send(ctx, &mailer.Message{
    To:      []string{"user@example.com"},
    Subject: "Your receipt",
    Text:    "Thanks. Your receipt is attached.",
    HTML:    `<img src="cid:logo"><p>Thanks. Your receipt is attached.</p>`,
    Attachments: []mailer.Attachment{
        {Filename: "receipt.pdf", Data: pdfBytes},
        {Filename: "logo.png", Data: logoBytes, ContentID: "logo"},
    },
})
```

`ContentType` is guessed from the file extension when left empty, falling back
to `application/octet-stream`.

### Batches

`SendMany` delivers over a single connection instead of paying for a handshake
per message. It returns a slice parallel to the input, nil where the message
was accepted, so one rejected address does not cost the batch.

```go
for i, err := range sender.SendMany(ctx, msgs) {
    if err != nil {
        log.Printf("message %d to %v: %v", i, msgs[i].To, err)
    }
}
```

Set `Config.RateLimit` to pace them. It is a strict pacer, not a token bucket:
it never bursts, because a burst against SES buys a throttling error.

```go
cfg.RateLimit = 10 // messages per second; fractions are fine (0.2 = one per 5s)
```

## Configuration

| Field | Default | Notes |
|---|---|---|
| `Host` | — | e.g. `mailer.SESHost("ap-southeast-2")` |
| `Port` | `587` | `465` and `2465` use implicit TLS; others use STARTTLS |
| `Username`, `Password` | — | leave empty to skip authentication |
| `From` | — | default sender; must be a verified SES identity |
| `Timeout` | 30s | **idle** timeout, refreshed per read/write |
| `RateLimit` | 0 | messages per second; 0 means unlimited |
| `LocalName` | `localhost` | the EHLO name |
| `AllowInsecure` | `false` | permit sending without TLS |
| `TLSConfig` | nil | `ServerName` is filled in from `Host` |

`Timeout` is an idle timeout rather than a total one, so it bounds a stalled
conversation without capping a long legitimate batch.

`AllowInsecure` is off by default. Without it, a server that does not offer
STARTTLS is an error rather than a silent downgrade that would put the
password on the wire in clear text. Turn it on only for a local test relay
such as Mailpit.

## Amazon SES

- Host is `email-smtp.<region>.amazonaws.com` — use `mailer.SESHost(region)`.
- `Username` and `Password` are SES **SMTP credentials**, not IAM access keys.
  Generate them in the SES console; the password is derived from an IAM secret
  key by an HMAC and cannot be recovered afterwards.
- `From` must be a verified SES identity (an address or a whole domain).
- A new account sits in the **SES sandbox**: it can only send to verified
  addresses, and is capped at 200 messages per 24 hours and 1 per second. Set
  `RateLimit` to stay under the per-second cap.
- Production quotas are per-account and shown in the SES console. Read them
  there rather than discovering them by being throttled.

```go
cfg := mailer.Config{
    Host:      mailer.SESHost("ap-southeast-2"),
    Username:  os.Getenv("MYAPP_SMTP_USERNAME"),
    Password:  os.Getenv("MYAPP_SMTP_PASSWORD"),
    From:      "noreply@example.com",
    RateLimit: 1, // sandbox
}
```

## Testing a consuming application

`MemorySender` records messages instead of sending them, and still composes
each one, so a test catches a caller assembling an invalid message.

```go
var sent mailer.MemorySender
svc := NewService(&sent)

svc.RequestVerification(ctx, "user@example.com")

if got := sent.Sent(); len(got) != 1 || got[0].To[0] != "user@example.com" {
    t.Fatalf("Sent() = %+v", got)
}
```

Set `sent.Err` to make every send fail, to exercise error handling.

## Safety

- Header injection is **rejected**, not stripped. Addresses go through
  `net/mail.ParseAddress`; `Subject` and custom headers are checked for line
  breaks. A newline in a header is an error, because a caller that got user
  input into a header has a bug worth surfacing.
- `Bcc` recipients receive the message but are never named in a header.
- `Message.Headers` rejects keys the builder writes itself, rather than
  silently duplicating or ignoring them.
- TLS is required unless explicitly waived.

## Development

```
make test     # vet + tests
make race     # the limiter and MemorySender carry shared state
make cover
make lint     # gofmt check + vet
```

`smtp_test.go` runs a minimal SMTP server in-process, which is what makes it
possible to test STARTTLS negotiation, the plaintext refusal, connection reuse
across a batch, the idle timeout against a stalled server, and cancellation.

## Topics

`go` · `golang` · `smtp` · `email` · `ses` · `stdlib` · `zero-dependencies` ·
`mailer`
