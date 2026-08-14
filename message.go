package mailer

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

// maxHeaderLine is RFC 5322's hard limit on a header line, excluding CRLF.
const maxHeaderLine = 998

// Message is a single outbound email.
//
// Supply Text, HTML, or both. Supplying both produces a multipart/alternative
// so the recipient's client picks whichever it can render — and sending a
// plain-text alternative alongside HTML measurably helps deliverability, so
// prefer both.
type Message struct {
	// From overrides Config.From for this message. Usually empty.
	From string

	// To, Cc and Bcc are RFC 5322 addresses: either "user@example.com" or
	// "Display Name <user@example.com>". Bcc recipients receive the message
	// but are not named in any header.
	To  []string
	Cc  []string
	Bcc []string

	// ReplyTo sets the Reply-To header. Optional.
	ReplyTo string

	Subject string

	// Text is the plain-text body; HTML is the HTML body. At least one is
	// required.
	Text string
	HTML string

	Attachments []Attachment

	// Headers adds extra headers. Keys that the builder writes itself (From,
	// To, Cc, Bcc, Subject, Date, Message-ID, MIME-Version and the Content-*
	// pair) are rejected rather than silently duplicated or ignored.
	Headers map[string]string

	// Date sets the Date header. Zero means time.Now.
	Date time.Time

	// MessageID sets the Message-ID header, without angle brackets. Zero means
	// a random one is generated using the From address's domain.
	MessageID string
}

// Attachment is a file carried by the message.
//
// Data is held in memory, which suits email-sized payloads — SES caps a raw
// message at 40 MB including base64 overhead, so streaming would buy little.
type Attachment struct {
	// Filename is the name offered to the recipient. Non-ASCII names are
	// encoded per RFC 2231.
	Filename string

	// ContentType is the MIME type. When empty it is guessed from the
	// filename extension, falling back to application/octet-stream.
	ContentType string

	Data []byte

	// ContentID, when set, marks this part as an inline resource rather than
	// an attachment, referenced from the HTML body as <img src="cid:the-id">.
	// The message is then wrapped in a multipart/related.
	ContentID string
}

// inline reports whether this part is a cid:-referenced resource.
func (a Attachment) inline() bool { return a.ContentID != "" }

// Recipients returns the envelope recipients — To, Cc and Bcc as bare
// addresses with any display name stripped. This is what goes in SMTP
// RCPT TO commands, as distinct from what appears in the headers.
func (m *Message) Recipients() ([]string, error) {
	var out []string
	for _, group := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, s := range group {
			a, err := mail.ParseAddress(s)
			if err != nil {
				return nil, fmt.Errorf("mailer: recipient %q: %w", s, err)
			}
			out = append(out, a.Address)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("mailer: message has no recipients")
	}
	return out, nil
}

// Build assembles the complete RFC 5322 message: headers followed by the MIME
// body tree. defaultFrom is used when Message.From is empty.
//
// It is exported deliberately. The result is a self-contained raw message, so
// it can be handed to any transport (including a future SES SendRawEmail call)
// and, more usefully day to day, the composition can be tested without a
// server in the loop.
func (m *Message) Build(defaultFrom string) ([]byte, error) {
	from := m.From
	if from == "" {
		from = defaultFrom
	}
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("mailer: From address %q: %w", from, err)
	}
	if m.Text == "" && m.HTML == "" {
		return nil, errors.New("mailer: message has neither a Text nor an HTML body")
	}
	if _, err := m.Recipients(); err != nil {
		return nil, err
	}

	l, err := m.plan()
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	if err := m.writeHeaders(&b, fromAddr, l); err != nil {
		return nil, err
	}
	b.WriteString("\r\n")
	if err := m.writeMixed(&b, l); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Headers
// ---------------------------------------------------------------------------

// reservedHeaders are written by the builder itself; a caller-supplied copy in
// Message.Headers is an error rather than a silent no-op.
var reservedHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true,
	"subject": true, "date": true, "message-id": true,
	"mime-version": true, "content-type": true,
	"content-transfer-encoding": true,
}

func (m *Message) writeHeaders(b *bytes.Buffer, from *mail.Address, l layout) error {
	write := func(key, value string) error {
		if len(key)+2+len(value) > maxHeaderLine {
			return fmt.Errorf("mailer: %s header exceeds the %d-octet line limit", key, maxHeaderLine)
		}
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteString("\r\n")
		return nil
	}

	if err := write("From", from.String()); err != nil {
		return err
	}
	for _, h := range []struct {
		key  string
		list []string
	}{{"To", m.To}, {"Cc", m.Cc}} {
		if len(h.list) == 0 {
			continue
		}
		v, err := formatAddrList(h.list)
		if err != nil {
			return fmt.Errorf("mailer: %s header: %w", h.key, err)
		}
		if err := write(h.key, v); err != nil {
			return err
		}
	}
	// Bcc is intentionally absent: naming blind recipients in a header that
	// every recipient can read defeats the point.

	if m.ReplyTo != "" {
		a, err := mail.ParseAddress(m.ReplyTo)
		if err != nil {
			return fmt.Errorf("mailer: Reply-To address %q: %w", m.ReplyTo, err)
		}
		if err := write("Reply-To", a.String()); err != nil {
			return err
		}
	}

	// RFC 2047 encoding turns a non-ASCII subject into ASCII encoded-words,
	// splitting and folding them when they run long. Plain ASCII passes
	// through untouched.
	if err := write("Subject", mime.QEncoding.Encode("utf-8", m.Subject)); err != nil {
		return err
	}

	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	if err := write("Date", date.Format(time.RFC1123Z)); err != nil {
		return err
	}

	msgID := m.MessageID
	if msgID == "" {
		id, err := newMessageID(from.Address)
		if err != nil {
			return err
		}
		msgID = id
	}
	if err := write("Message-ID", "<"+msgID+">"); err != nil {
		return err
	}

	for k, v := range m.Headers {
		if reservedHeaders[strings.ToLower(k)] {
			return fmt.Errorf("mailer: header %q is set by the builder and cannot be overridden via Headers", k)
		}
		if strings.ContainsAny(k, "\r\n:") || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("mailer: header %q contains a line break or colon", k)
		}
		if err := write(k, v); err != nil {
			return err
		}
	}

	if err := write("MIME-Version", "1.0"); err != nil {
		return err
	}
	if err := write("Content-Type", m.mixedType(l)); err != nil {
		return err
	}
	// When there is no multipart wrapper at all, the single body is written
	// straight after the headers, so its transfer encoding belongs up here.
	if l.isBare() {
		if err := write("Content-Transfer-Encoding", "quoted-printable"); err != nil {
			return err
		}
	}
	return nil
}

// formatAddrList parses and re-renders an address list, which both validates
// it and applies RFC 2047 encoding to any non-ASCII display name. Parsing also
// rejects embedded CR/LF, closing off header injection.
func formatAddrList(in []string) (string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		a, err := mail.ParseAddress(s)
		if err != nil {
			return "", fmt.Errorf("address %q: %w", s, err)
		}
		out = append(out, a.String())
	}
	return strings.Join(out, ", "), nil
}

func newMessageID(fromAddr string) (string, error) {
	domain := "localhost"
	if i := strings.LastIndex(fromAddr, "@"); i >= 0 && i+1 < len(fromAddr) {
		domain = fromAddr[i+1:]
	}
	tok, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return tok + "@" + domain, nil
}

// ---------------------------------------------------------------------------
// MIME structure
// ---------------------------------------------------------------------------

// layout records which MIME container levels this message needs. An empty
// boundary means that level is absent. The full nesting, when everything is
// present, is:
//
//	multipart/mixed
//	  multipart/related
//	    multipart/alternative
//	      text/plain
//	      text/html
//	    inline part (cid:)…
//	  attachment…
//
// Each level is elided when it would hold only one child, so a plain-text
// message with no attachments is a bare text/plain with no boundaries at all.
type layout struct {
	mixed string // content + attachments
	rel   string // content + inline cid: resources
	alt   string // text/plain + text/html
}

func (l layout) isBare() bool { return l.mixed == "" && l.rel == "" && l.alt == "" }

func (m *Message) plan() (layout, error) {
	var l layout
	var err error
	if len(m.parts(false)) > 0 {
		if l.mixed, err = randomHex(16); err != nil {
			return l, err
		}
	}
	if len(m.parts(true)) > 0 {
		if l.rel, err = randomHex(16); err != nil {
			return l, err
		}
	}
	if m.Text != "" && m.HTML != "" {
		if l.alt, err = randomHex(16); err != nil {
			return l, err
		}
	}
	return l, nil
}

// parts returns the attachments that are (or are not) inline resources.
func (m *Message) parts(inline bool) []Attachment {
	var out []Attachment
	for _, a := range m.Attachments {
		if a.inline() == inline {
			out = append(out, a)
		}
	}
	return out
}

// leafType is the media type of the single body part, used when there is no
// multipart/alternative to hold both bodies.
func (m *Message) leafType() string {
	if m.Text == "" {
		return "text/html"
	}
	return "text/plain"
}

func (m *Message) leafBody() string {
	if m.Text == "" {
		return m.HTML
	}
	return m.Text
}

// The three *Type methods each describe the subtree rooted at their level,
// falling through to the next level down when their own container is absent.

func (m *Message) mixedType(l layout) string {
	if l.mixed == "" {
		return m.relType(l)
	}
	return mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": l.mixed})
}

func (m *Message) relType(l layout) string {
	if l.rel == "" {
		return m.altType(l)
	}
	// The type parameter names the media type of the root part, which is what
	// a client renders and resolves cid: references against.
	root, _, err := mime.ParseMediaType(m.altType(l))
	if err != nil {
		root = "multipart/alternative"
	}
	return mime.FormatMediaType("multipart/related", map[string]string{
		"type":     root,
		"boundary": l.rel,
	})
}

func (m *Message) altType(l layout) string {
	if l.alt == "" {
		return mime.FormatMediaType(m.leafType(), map[string]string{"charset": "utf-8"})
	}
	return mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": l.alt})
}

// ---------------------------------------------------------------------------
// Body writers, outermost level first
// ---------------------------------------------------------------------------

func (m *Message) writeMixed(w io.Writer, l layout) error {
	if l.mixed == "" {
		return m.writeRelated(w, l)
	}
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(l.mixed); err != nil {
		return err
	}
	part, err := mw.CreatePart(headerFor(m.relType(l), l.rel == "" && l.alt == ""))
	if err != nil {
		return err
	}
	if err := m.writeRelated(part, l); err != nil {
		return err
	}
	for _, a := range m.parts(false) {
		if err := writeAttachment(mw, a); err != nil {
			return err
		}
	}
	return mw.Close()
}

func (m *Message) writeRelated(w io.Writer, l layout) error {
	if l.rel == "" {
		return m.writeAlternative(w, l)
	}
	rw := multipart.NewWriter(w)
	if err := rw.SetBoundary(l.rel); err != nil {
		return err
	}
	part, err := rw.CreatePart(headerFor(m.altType(l), l.alt == ""))
	if err != nil {
		return err
	}
	if err := m.writeAlternative(part, l); err != nil {
		return err
	}
	for _, a := range m.parts(true) {
		if err := writeAttachment(rw, a); err != nil {
			return err
		}
	}
	return rw.Close()
}

func (m *Message) writeAlternative(w io.Writer, l layout) error {
	if l.alt == "" {
		// Either the whole message is one bare body (headers already written
		// by writeHeaders) or this is a leaf part whose headers the parent
		// wrote when it created the part. Either way, just the encoded body.
		return writeQuotedPrintable(w, m.leafBody())
	}
	aw := multipart.NewWriter(w)
	if err := aw.SetBoundary(l.alt); err != nil {
		return err
	}
	// Least-preferred alternative first, per RFC 2046: clients pick the last
	// one they can render.
	for _, p := range []struct{ ctype, body string }{
		{"text/plain", m.Text},
		{"text/html", m.HTML},
	} {
		part, err := aw.CreatePart(headerFor(
			mime.FormatMediaType(p.ctype, map[string]string{"charset": "utf-8"}), true))
		if err != nil {
			return err
		}
		if err := writeQuotedPrintable(part, p.body); err != nil {
			return err
		}
	}
	return aw.Close()
}

// headerFor builds the MIME headers for a child part. leaf marks a part whose
// content is a quoted-printable text body rather than a nested multipart.
func headerFor(contentType string, leaf bool) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	if leaf {
		h.Set("Content-Transfer-Encoding", "quoted-printable")
	}
	return h
}

// writeQuotedPrintable encodes a body. The standard library's writer also
// normalises bare LF and lone CR to CRLF and wraps long lines, so callers need
// not pre-process the text.
func writeQuotedPrintable(w io.Writer, body string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := io.WriteString(qp, body); err != nil {
		return err
	}
	return qp.Close()
}

func writeAttachment(w *multipart.Writer, a Attachment) error {
	if a.Filename == "" && a.ContentID == "" {
		return errors.New("mailer: attachment needs a Filename or a ContentID")
	}
	if strings.ContainsAny(a.Filename, "\r\n") || strings.ContainsAny(a.ContentID, "\r\n<>") {
		return fmt.Errorf("mailer: attachment %q has an invalid Filename or ContentID", a.Filename)
	}

	ctype := a.ContentType
	if ctype == "" {
		ctype = mime.TypeByExtension(filepath.Ext(a.Filename))
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt, params = "application/octet-stream", map[string]string{}
	}
	if params == nil {
		params = map[string]string{}
	}

	disposition := "attachment"
	if a.inline() {
		disposition = "inline"
	}
	dparams := map[string]string{}
	if a.Filename != "" {
		// The name parameter on Content-Type is obsolete but still consulted
		// by some older clients; filename on Content-Disposition is the one
		// that matters. FormatMediaType applies RFC 2231 encoding when the
		// value is not plain ASCII.
		params["name"] = a.Filename
		dparams["filename"] = a.Filename
	}

	h := textproto.MIMEHeader{}
	h.Set("Content-Type", mime.FormatMediaType(mt, params))
	h.Set("Content-Transfer-Encoding", "base64")
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, dparams))
	if a.inline() {
		// Assigned directly rather than via Set, which would canonicalise the
		// key to "Content-Id". Header names are case-insensitive, but RFC 2392
		// spells it "Content-ID" and so does every other mailer; CreatePart
		// writes map keys verbatim, so this costs nothing.
		h["Content-ID"] = []string{"<" + a.ContentID + ">"}
	}

	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	wrap := &wrapWriter{w: part, width: 76}
	enc := base64.NewEncoder(base64.StdEncoding, wrap)
	if _, err := enc.Write(a.Data); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return wrap.flush()
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// wrapWriter breaks a stream into CRLF-terminated lines of at most width
// bytes. RFC 2045 caps an encoded line at 76 characters and base64.Encoder
// does no wrapping of its own.
type wrapWriter struct {
	w     io.Writer
	width int
	col   int
}

func (t *wrapWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if room := t.width - t.col; len(chunk) > room {
			chunk = chunk[:room]
		}
		n, err := t.w.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		t.col += n
		p = p[n:]
		if t.col >= t.width {
			if _, err := io.WriteString(t.w, "\r\n"); err != nil {
				return written, err
			}
			t.col = 0
		}
	}
	return written, nil
}

// flush terminates a partial final line.
func (t *wrapWriter) flush() error {
	if t.col == 0 {
		return nil
	}
	t.col = 0
	_, err := io.WriteString(t.w, "\r\n")
	return err
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mailer: read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
