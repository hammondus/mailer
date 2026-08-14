# Design decisions

Why this module looks the way it does. Read alongside the package
documentation in `mailer.go`, which covers *how* to use it.

## Why the module exists at all

It replaces a hand-rolled `internal/mailer` copied between projects. The
deduplication was never the point — the original was 138 lines of standard
library, and three copies of that is not a crisis. What justified extracting
it was that each copy carried the same five defects, and fixing them in one
place with tests is worth more than the copy-paste saving:

1. **No deadline after dial.** `net.Dialer{Timeout: …}` bounds only the TCP
   connect. Once connected, nothing stopped a server that accepted and then
   went silent from hanging a request handler indefinitely — precisely the
   failure the original code's own comment claimed to prevent. See
   `idleConn` below.
2. **Silent TLS downgrade.** The original did `if ok, _ := c.Extension(
   "STARTTLS"); ok { … }`, so a server that simply did not advertise STARTTLS
   caused the SES password to go out in clear text with no error. Now that is
   a refusal unless `Config.AllowInsecure` is set.
3. **Subject not RFC 2047 encoded.** A non-ASCII subject emitted raw UTF-8 in
   a header, which is not conformant and renders as mojibake in some clients.
4. **No `context.Context`.** A send could not be cancelled when the client
   disconnected.
5. **Single recipient, no Cc/Bcc, no attachments, no HTML.**

## Standard library only

There are no dependencies and there is no intention of adding any. Everything
needed is in `net/smtp`, `mime`, `mime/multipart`, `mime/quotedprintable`,
`net/mail` and `net/textproto`.

## SMTP rather than the SES API

SES was reached through its SMTP endpoint before and still is. The
alternatives were weighed:

| Approach | Dependencies | Gains |
|---|---|---|
| **SMTP** (chosen) | none | works against any provider |
| SESv2 via `aws-sdk-go-v2` | ~10 modules | SES `MessageId`, IAM instance roles |
| SESv2 over `net/http` + hand-rolled SigV4 | none | same, but SigV4 bugs fail opaquely |

SMTP wins because it was already proven in production, costs nothing in
dependencies, and the SES `MessageId` only matters if bounce and complaint
notifications from SNS are being correlated back to individual sends. Nothing
here does that yet.

The one thing that would change the decision: deploying somewhere with an EC2
or ECS instance role, where the API route removes a stored long-lived
credential entirely. If that day comes, `Message.Build` already produces a
complete raw message suitable for `SendRawEmail`, so the change is a new
`Sender` implementation rather than a rewrite.

## The module is not SES-specific, and is not named as though it were

The module path is `github.com/hammondus/mailer`. Nothing in the code depends
on Amazon: SES is a hostname. The same code works against Postmark, Mailgun,
Fastmail or a local Postfix. Naming it for one vendor would have been a lie
that got more misleading over time. SES-specific knowledge lives in one
function (`SESHost`) and one documentation section.

The name also has to sit alongside `github.com/hammondus/gomail`, the FileMaker
email gateway that consumes this module. `mailer` and `gomail` are far enough
apart to read unambiguously in a `go.mod`; `go-mailer` and `gomail` were not.

## Configuration is a struct, not environment variables

`Config` is a plain struct. `ConfigFromEnv(prefix)` exists but is opt-in and
takes the prefix as a parameter, so each application keeps its own naming
convention (`AIRPORTINFO_SMTP_HOST` and friends) instead of the library
imposing one.

`Timeout`, `RateLimit`, `AllowInsecure` and `TLSConfig` are deliberately *not*
read from the environment. They are deployment decisions that belong in code,
and `AllowInsecure` in particular should never be one environment variable
away from disabling transport security in production.

## `idleConn`: an idle timeout, not an absolute deadline

`net.Conn` offers absolute deadlines. A single deadline covering the whole
conversation would be wrong for `SendMany`, where a legitimate batch can run
for minutes. `idleConn` refreshes the deadline before every read and write,
which converts it into "give up if nothing moves for `Timeout`". That bounds
the stall without capping the batch.

Cancellation is layered on separately: `net/smtp` predates `context`, so a
goroutine closes the connection when the context is done, which surfaces as a
read or write error. The error is then rewritten to wrap `ctx.Err()` so
callers can use `errors.Is`.

## Header injection is rejected, not stripped

The original silently removed CR and LF from header values, turning
`Evil\r\nBcc: victim@z` into `EvilBcc: victim@z`. That is safe but dishonest:
it quietly mangles the caller's data and hides a bug.

Here, addresses go through `net/mail.ParseAddress` — which rejects embedded
line breaks as a side effect of parsing correctly — and `Subject` plus custom
headers are checked explicitly. A newline in a header is an error. A caller
that got user input into a header has a bug worth surfacing.

`Message.Headers` also rejects keys the builder writes itself (`From`, `To`,
`Subject`, `Content-Type` and so on) rather than silently duplicating or
ignoring them.

## MIME structure: every container level is elided when redundant

The full nesting, when a message has both bodies, inline images and
attachments, is:

```
multipart/mixed
  multipart/related
    multipart/alternative
      text/plain
      text/html
    inline part (cid:)…
  attachment…
```

Each level is dropped when it would hold only one child, so a plain-text
message with no attachments is a bare `text/plain` with no boundaries at all.
This matters: some clients render a single-part `multipart/mixed` as an empty
message with a mysterious attachment, and gratuitous nesting hurts spam
scores.

The decision is made once in `plan()`, which returns a `layout` of three
boundary strings; an empty string means that level is absent. The three
`*Type` methods each describe the subtree rooted at their level and fall
through to the next level down when their own container is absent, which is
what keeps the `Content-Type` header and the actual body structure from
drifting apart.

`text/plain` is written before `text/html` because RFC 2046 has clients pick
the *last* alternative they can render, so least-preferred goes first.

## Rate limiting is a strict pacer, not a token bucket

`Config.RateLimit` spaces messages evenly and never allows a burst. A token
bucket would let an idle sender bank allowance and spend it at once, which
against SES buys a `454 Throttling failure` and, repeated, a reputation
problem. Being slightly slower than the cap is the safe direction to be wrong
in.

A cancelled caller forfeits its reserved slot rather than releasing it back,
which leaves a gap in the schedule. That also errs towards sending too slowly,
so it is left alone rather than fixed with more machinery.

The SES sandbox caps sending at 1 message per second and 200 per 24 hours.
Production quotas are per-account and shown in the SES console — read them
there rather than discovering them by being throttled.

## `SendMany` shares one connection and returns a parallel error slice

Reconnecting per message costs a TCP and TLS handshake each time, which is
wasteful for a batch and unfriendly to the relay.

The return type is `[]error` parallel to the input rather than a single error,
because "which of these 400 messages failed" is the question a caller actually
has. `errors.Join(errs...)` collapses it for callers who do not care.

A rejected recipient is a per-message failure; the code issues an SMTP `RSET`
and continues, so one bad address does not cost the batch. If the `RSET`
itself fails the connection is unusable, and every remaining message is failed
with that error rather than being silently dropped.

## `Build` is exported

`Message.Build` returns the complete raw message. It is exported because it
makes composition testable without a server in the loop — which is how the
whole MIME layer is tested — and because the result can be handed to any other
transport, including a future SES `SendRawEmail`.

## Attachments are `[]byte`, not `io.Reader`

SES caps a raw message at 40 MB including base64 overhead, so streaming buys
little and costs a much more awkward API. If a payload is large enough for
that to hurt, it should be a link rather than an attachment.

## Three `Sender` implementations

`Sender` is a one-method interface so applications can depend on it and swap
by configuration:

- `SMTPSender` — the real one.
- `LogSender` — for when `Config.Configured()` is false, so local development
  runs the whole surrounding path and the verification link is visible in the
  logs rather than lost. It still *composes* the message, so a malformed one
  fails in development exactly as it would in production.
- `MemorySender` — records messages for the tests of consuming applications.
  It also composes, for the same reason.

`LogSender` takes a `*slog.Logger` rather than using the global `log` package,
because a library that writes to its host's global logger is a nuisance.

## Testing approach

The MIME tests parse the built message with `net/mail` and `mime/multipart`
and assert on the resulting tree, rather than matching substrings. Substring
matching on email is a trap — `mail.Address.String()` renders
`user@example.com` as `<user@example.com>`, so the obvious assertion fails on
correct output.

`smtp_test.go` runs a real, if minimal, SMTP server in-process. That is what
makes it possible to test the things that actually matter and cannot be tested
any other way: STARTTLS negotiation against a generated certificate, the
refusal to send in the clear, connection reuse across a batch, recovery from a
rejected recipient, the idle timeout against a deliberately stalled server,
and context cancellation.
