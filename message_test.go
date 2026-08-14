package mailer

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// build is a shorthand that fails the test on a composition error.
func build(t *testing.T, m *Message) []byte {
	t.Helper()
	raw, err := m.Build("sender@example.com")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return raw
}

// parse splits a built message into its headers and body reader.
func parse(t *testing.T, raw []byte) (mail.Header, io.Reader) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v\n---\n%s", err, raw)
	}
	return msg.Header, msg.Body
}

// describe renders the MIME tree as a compact string, e.g.
// "multipart/mixed[multipart/alternative[text/plain,text/html],application/pdf]".
// Structural assertions against a real parse beat matching on substrings.
func describe(t *testing.T, contentType string, body io.Reader) string {
	t.Helper()
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", contentType, err)
	}
	if !strings.HasPrefix(mt, "multipart/") {
		return mt
	}
	mr := multipart.NewReader(body, params["boundary"])
	var kids []string
	for {
		// NextRawPart leaves Content-Transfer-Encoding in place; NextPart
		// strips it after decoding, which would hide it from assertions.
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextRawPart: %v", err)
		}
		kids = append(kids, describe(t, p.Header.Get("Content-Type"), p))
	}
	return mt + "[" + strings.Join(kids, ",") + "]"
}

func TestBuildStructure(t *testing.T) {
	pdf := Attachment{Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4")}
	png := Attachment{Filename: "logo.png", ContentType: "image/png", Data: []byte("\x89PNG"), ContentID: "logo"}

	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{
			name: "text only is a bare part",
			msg:  Message{To: []string{"a@example.com"}, Text: "hi"},
			want: "text/plain",
		},
		{
			name: "html only is a bare part",
			msg:  Message{To: []string{"a@example.com"}, HTML: "<p>hi</p>"},
			want: "text/html",
		},
		{
			name: "both bodies become alternative",
			msg:  Message{To: []string{"a@example.com"}, Text: "hi", HTML: "<p>hi</p>"},
			want: "multipart/alternative[text/plain,text/html]",
		},
		{
			name: "attachment adds mixed",
			msg:  Message{To: []string{"a@example.com"}, Text: "hi", Attachments: []Attachment{pdf}},
			want: "multipart/mixed[text/plain,application/pdf]",
		},
		{
			name: "both bodies plus attachment nests alternative inside mixed",
			msg:  Message{To: []string{"a@example.com"}, Text: "hi", HTML: "<p>hi</p>", Attachments: []Attachment{pdf}},
			want: "multipart/mixed[multipart/alternative[text/plain,text/html],application/pdf]",
		},
		{
			name: "inline resource adds related",
			msg:  Message{To: []string{"a@example.com"}, HTML: `<img src="cid:logo">`, Attachments: []Attachment{png}},
			want: "multipart/related[text/html,image/png]",
		},
		{
			name: "the full tree",
			msg: Message{
				To: []string{"a@example.com"}, Text: "hi", HTML: `<img src="cid:logo">`,
				Attachments: []Attachment{pdf, png},
			},
			want: "multipart/mixed[multipart/related[multipart/alternative[text/plain,text/html],image/png],application/pdf]",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hdr, body := parse(t, build(t, &c.msg))
			if got := describe(t, hdr.Get("Content-Type"), body); got != c.want {
				t.Errorf("structure =\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

func TestBuildRoundTripsBodies(t *testing.T) {
	const text = "line one\nline two, with an = sign and a café\n"
	const html = "<p>café &amp; crème</p>"

	hdr, body := parse(t, build(t, &Message{
		To: []string{"a@example.com"}, Subject: "s", Text: text, HTML: html,
	}))

	_, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(body, params["boundary"])

	// NextPart decodes quoted-printable transparently, which is exactly the
	// decode a receiving client performs.
	for _, want := range []string{text, html} {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		got, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		// Quoted-printable normalises line endings; compare on that basis.
		if normalise(string(got)) != normalise(want) {
			t.Errorf("body round trip:\n got %q\nwant %q", got, want)
		}
	}
}

func normalise(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func TestBuildEncodesNonASCIISubject(t *testing.T) {
	hdr, _ := parse(t, build(t, &Message{
		To: []string{"a@example.com"}, Subject: "Café — your key is ready", Text: "hi",
	}))

	raw := hdr.Get("Subject")
	for _, r := range raw {
		if r > 127 {
			t.Fatalf("Subject header carries raw non-ASCII: %q", raw)
		}
	}
	// A conforming reader must get the original text back.
	got, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if want := "Café — your key is ready"; got != want {
		t.Errorf("decoded subject = %q, want %q", got, want)
	}
}

func TestBuildOmitsBccFromHeaders(t *testing.T) {
	m := &Message{
		To:   []string{"visible@example.com"},
		Bcc:  []string{"hidden@example.com"},
		Text: "hi",
	}
	raw := build(t, m)
	if bytes.Contains(bytes.ToLower(raw), []byte("hidden@example.com")) {
		t.Errorf("Bcc address leaked into the message:\n%s", raw)
	}
	// It must still be an envelope recipient, or it would never arrive.
	rcpts, err := m.Recipients()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"visible@example.com", "hidden@example.com"}; !equal(rcpts, want) {
		t.Errorf("Recipients() = %v, want %v", rcpts, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
	}{
		{"via To", Message{To: []string{"a@example.com\r\nBcc: victim@example.com"}, Text: "hi"}},
		{"via From", Message{From: "a@example.com\r\nBcc: victim@example.com", To: []string{"b@example.com"}, Text: "hi"}},
		{"via Reply-To", Message{To: []string{"a@example.com"}, ReplyTo: "x@y.com\r\nBcc: v@z.com", Text: "hi"}},
		{"via custom header value", Message{To: []string{"a@example.com"}, Text: "hi",
			Headers: map[string]string{"X-Thing": "ok\r\nBcc: victim@example.com"}}},
		{"via custom header name", Message{To: []string{"a@example.com"}, Text: "hi",
			Headers: map[string]string{"X-Thing\r\nBcc": "victim@example.com"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Rejecting outright beats silently stripping: a caller that
			// smuggled a newline in has a bug worth surfacing.
			if _, err := c.msg.Build("sender@example.com"); err == nil {
				t.Fatal("Build accepted a header containing CRLF")
			}
		})
	}
}

func TestBuildRejectsReservedHeaderOverride(t *testing.T) {
	m := &Message{To: []string{"a@example.com"}, Text: "hi",
		Headers: map[string]string{"Content-Type": "text/html"}}
	if _, err := m.Build("sender@example.com"); err == nil {
		t.Fatal("Build allowed Content-Type to be set via Headers")
	}
}

func TestBuildRequiresBodyAndRecipient(t *testing.T) {
	if _, err := (&Message{To: []string{"a@example.com"}}).Build("s@example.com"); err == nil {
		t.Error("Build accepted a message with no body")
	}
	if _, err := (&Message{Text: "hi"}).Build("s@example.com"); err == nil {
		t.Error("Build accepted a message with no recipients")
	}
	if _, err := (&Message{To: []string{"a@example.com"}, Text: "hi"}).Build(""); err == nil {
		t.Error("Build accepted a message with no From")
	}
}

func TestAttachmentEncoding(t *testing.T) {
	// Large enough to force several base64 lines.
	data := bytes.Repeat([]byte("abcdefghij"), 40)
	hdr, body := parse(t, build(t, &Message{
		To: []string{"a@example.com"}, Text: "see attached",
		Attachments: []Attachment{{Filename: "data.bin", Data: data}},
	}))

	_, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(body, params["boundary"])
	if _, err := mr.NextRawPart(); err != nil { // skip the text body
		t.Fatal(err)
	}
	p, err := mr.NextRawPart()
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Header.Get("Content-Transfer-Encoding"); got != "base64" {
		t.Errorf("Content-Transfer-Encoding = %q, want base64", got)
	}
	if got := p.Header.Get("Content-Disposition"); !strings.Contains(got, `filename=data.bin`) {
		t.Errorf("Content-Disposition = %q, want a filename parameter", got)
	}
	// No extension mapping for .bin, so it must fall back.
	if got := p.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/octet-stream") {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}

	encoded, err := io.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimRight(string(encoded), "\r\n"), "\r\n") {
		if len(line) > 76 {
			t.Errorf("base64 line is %d chars, over the 76 limit: %q", len(line), line)
		}
	}
	got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
	if err != nil {
		t.Fatalf("decode attachment: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("attachment did not survive the round trip")
	}
}

func TestInlineAttachmentGetsContentID(t *testing.T) {
	hdr, body := parse(t, build(t, &Message{
		To: []string{"a@example.com"}, HTML: `<img src="cid:logo@app">`,
		Attachments: []Attachment{{Filename: "logo.png", ContentType: "image/png",
			Data: []byte("\x89PNG"), ContentID: "logo@app"}},
	}))

	mt, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mt != "multipart/related" {
		t.Fatalf("top-level type = %q, want multipart/related", mt)
	}
	// The type parameter tells a client which part to render as the root.
	if params["type"] != "text/html" {
		t.Errorf("related type parameter = %q, want text/html", params["type"])
	}

	mr := multipart.NewReader(body, params["boundary"])
	if _, err := mr.NextRawPart(); err != nil {
		t.Fatal(err)
	}
	p, err := mr.NextRawPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Header.Get("Content-ID"); got != "<logo@app>" {
		t.Errorf("Content-ID = %q, want <logo@app>", got)
	}
	if got := p.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}
}

func TestInlineContentIDKeepsItsWireSpelling(t *testing.T) {
	// textproto canonicalisation would emit "Content-Id"; parsed headers
	// canonicalise on read too, so only the raw bytes can show the difference.
	raw := build(t, &Message{
		To: []string{"a@example.com"}, HTML: `<img src="cid:logo">`,
		Attachments: []Attachment{{Filename: "logo.png", ContentType: "image/png",
			Data: []byte("\x89PNG"), ContentID: "logo"}},
	})
	if !bytes.Contains(raw, []byte("Content-ID: <logo>")) {
		t.Errorf("Content-ID not spelled as RFC 2392 does:\n%s", raw)
	}
}

func TestBuildSetsDateAndMessageID(t *testing.T) {
	when := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	hdr, _ := parse(t, build(t, &Message{
		To: []string{"a@example.com"}, Text: "hi", Date: when,
	}))

	got, err := hdr.Date()
	if err != nil {
		t.Fatalf("Date header: %v", err)
	}
	if !got.Equal(when) {
		t.Errorf("Date = %v, want %v", got, when)
	}
	// A generated Message-ID must be bracketed and use the sender's domain.
	if id := hdr.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Errorf("Message-ID = %q, want <token@example.com>", id)
	}
}

func TestBuildPreservesDisplayNames(t *testing.T) {
	hdr, _ := parse(t, build(t, &Message{
		From: `"Bluesky Flying" <noreply@example.com>`,
		To:   []string{"Ada Lovelace <ada@example.com>"},
		Text: "hi",
	}))
	from, err := mail.ParseAddress(hdr.Get("From"))
	if err != nil {
		t.Fatalf("From header did not re-parse: %v", err)
	}
	if from.Name != "Bluesky Flying" || from.Address != "noreply@example.com" {
		t.Errorf("From = %+v, want Bluesky Flying <noreply@example.com>", from)
	}
}

func TestQuotedPrintableAvoidsLongLines(t *testing.T) {
	// A single very long line must be soft-wrapped, or the message violates
	// the 998-octet line limit and some relays will mangle it.
	raw := build(t, &Message{
		To: []string{"a@example.com"}, Text: strings.Repeat("word ", 500),
	})
	for line := range bytes.SplitSeq(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatalf("line of %d octets exceeds the RFC 5322 limit", len(line))
		}
	}
}

func TestWrapWriter(t *testing.T) {
	var b bytes.Buffer
	w := &wrapWriter{w: &b, width: 4}
	if _, err := io.WriteString(w, "abcdefghij"); err != nil {
		t.Fatal(err)
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "abcd\r\nefgh\r\nij\r\n"; got != want {
		t.Errorf("wrapWriter = %q, want %q", got, want)
	}
}

func TestQuotedPrintableWriterNormalisesLineEndings(t *testing.T) {
	// Documents the standard library behaviour the builder relies on: bodies
	// are passed through with bare LFs and come out as CRLF.
	var b bytes.Buffer
	if err := writeQuotedPrintable(&b, "a\nb"); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "a\r\nb" {
		t.Errorf("quoted-printable output = %q, want %q", got, "a\r\nb")
	}
	r := quotedprintable.NewReader(strings.NewReader(b.String()))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "a\r\nb" {
		t.Errorf("decoded = %q", out)
	}
}
