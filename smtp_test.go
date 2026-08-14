package mailer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal in-process SMTP server: enough of the protocol to
// drive the client through a real conversation, and nothing more.
type fakeServer struct {
	ln       net.Listener
	tlsCfg   *tls.Config // non-nil to advertise and accept STARTTLS
	stall    bool        // greet, then never respond again
	rejectTo string      // refuse RCPT TO for any address containing this

	mu       sync.Mutex
	conns    int
	auths    int
	messages [][]byte
}

func newFakeServer(t *testing.T, configure func(*fakeServer)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{ln: ln}
	if configure != nil {
		configure(s)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeServer) hostPort() (string, string) {
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	return host, port
}

// config returns a client Config pointed at this server.
func (s *fakeServer) config() Config {
	host, port := s.hostPort()
	return Config{
		Host:          host,
		Port:          port,
		From:          "sender@example.com",
		Timeout:       5 * time.Second,
		AllowInsecure: s.tlsCfg == nil,
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()

	if s.stall {
		io.WriteString(conn, "220 fake ESMTP\r\n")
		io.Copy(io.Discard, conn) // read everything, answer nothing
		return
	}

	secure := false
	tc := textproto.NewConn(conn)
	tc.PrintfLine("220 fake ESMTP")
	for {
		line, err := tc.ReadLine()
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "EHLO", "HELO":
			tc.PrintfLine("250-fake greets you")
			if s.tlsCfg != nil && !secure {
				tc.PrintfLine("250-STARTTLS")
			}
			tc.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			tc.PrintfLine("220 ready to start TLS")
			tconn := tls.Server(conn, s.tlsCfg)
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn, secure = tconn, true
			tc = textproto.NewConn(conn)
		case "AUTH":
			s.mu.Lock()
			s.auths++
			s.mu.Unlock()
			tc.PrintfLine("235 2.7.0 authentication succeeded")
		case "MAIL":
			tc.PrintfLine("250 2.1.0 ok")
		case "RCPT":
			if s.rejectTo != "" && strings.Contains(arg, s.rejectTo) {
				tc.PrintfLine("550 5.1.1 no such user")
			} else {
				tc.PrintfLine("250 2.1.5 ok")
			}
		case "DATA":
			tc.PrintfLine("354 end with <CRLF>.<CRLF>")
			body, err := tc.ReadDotBytes()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.messages = append(s.messages, body)
			s.mu.Unlock()
			tc.PrintfLine("250 2.0.0 ok")
		case "RSET":
			tc.PrintfLine("250 2.0.0 ok")
		case "QUIT":
			tc.PrintfLine("221 2.0.0 bye")
			return
		default:
			tc.PrintfLine("500 5.5.1 unrecognised command")
		}
	}
}

func (s *fakeServer) received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.messages...)
}

func (s *fakeServer) counts() (conns, auths int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, s.auths
}

func TestSendDeliversMessage(t *testing.T) {
	s := newFakeServer(t, nil)
	sender := NewSMTP(s.config())

	err := sender.Send(t.Context(), &Message{
		To:      []string{"user@example.com"},
		Subject: "Verify your key",
		Text:    "Open https://example.com/verify?t=abc",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := s.received()
	if len(got) != 1 {
		t.Fatalf("server received %d messages, want 1", len(got))
	}
	// Parse what arrived rather than matching substrings: addresses are
	// rendered in angle-addr form, so "To: user@example.com" would not match
	// even though the header is correct.
	msg, err := mail.ReadMessage(bytes.NewReader(got[0]))
	if err != nil {
		t.Fatalf("delivered message did not parse: %v\n%s", err, got[0])
	}
	if subject := msg.Header.Get("Subject"); subject != "Verify your key" {
		t.Errorf("Subject = %q", subject)
	}
	to, err := msg.Header.AddressList("To")
	if err != nil || len(to) != 1 || to[0].Address != "user@example.com" {
		t.Errorf("To = %v (%v), want user@example.com", to, err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	// The DATA dot-writer terminates the final line, so a trailing newline is
	// expected here even though the composed message has none.
	if want := "Open https://example.com/verify?t=abc"; strings.TrimRight(string(body), "\r\n") != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestSendAuthenticates(t *testing.T) {
	s := newFakeServer(t, nil)
	cfg := s.config()
	// PlainAuth only hands credentials to a TLS or loopback server; the fake
	// listens on 127.0.0.1, which qualifies.
	cfg.Username, cfg.Password = "AKIAEXAMPLE", "secret"
	sender := NewSMTP(cfg)

	if err := sender.Send(t.Context(), &Message{
		To: []string{"user@example.com"}, Text: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, auths := s.counts(); auths != 1 {
		t.Errorf("server saw %d AUTH commands, want 1", auths)
	}
}

func TestSendManyReusesOneConnection(t *testing.T) {
	s := newFakeServer(t, nil)
	sender := NewSMTP(s.config())

	msgs := []*Message{
		{To: []string{"a@example.com"}, Text: "one"},
		{To: []string{"b@example.com"}, Text: "two"},
		{To: []string{"c@example.com"}, Text: "three"},
	}
	for i, err := range sender.SendMany(t.Context(), msgs) {
		if err != nil {
			t.Errorf("message %d: %v", i, err)
		}
	}
	if got := len(s.received()); got != 3 {
		t.Errorf("server received %d messages, want 3", got)
	}
	if conns, _ := s.counts(); conns != 1 {
		t.Errorf("opened %d connections for a batch, want 1", conns)
	}
}

func TestSendManyContinuesPastARejectedRecipient(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer) { s.rejectTo = "bounce@example.com" })
	sender := NewSMTP(s.config())

	msgs := []*Message{
		{To: []string{"a@example.com"}, Text: "one"},
		{To: []string{"bounce@example.com"}, Text: "two"},
		{To: []string{"c@example.com"}, Text: "three"},
	}
	errs := sender.SendMany(t.Context(), msgs)

	if errs[0] != nil || errs[2] != nil {
		t.Errorf("good messages failed: %v, %v", errs[0], errs[2])
	}
	if errs[1] == nil {
		t.Error("rejected recipient did not produce an error")
	}
	// The point of the RSET: one bad address must not cost the whole batch.
	if got := len(s.received()); got != 2 {
		t.Errorf("server received %d messages, want 2", got)
	}
}

func TestSendRefusesPlaintextWithoutOptIn(t *testing.T) {
	s := newFakeServer(t, nil) // advertises no STARTTLS
	cfg := s.config()
	cfg.AllowInsecure = false
	sender := NewSMTP(cfg)

	err := sender.Send(t.Context(), &Message{To: []string{"a@example.com"}, Text: "hi"})
	if err == nil {
		t.Fatal("sent over an unencrypted connection without AllowInsecure")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("error should name the missing STARTTLS: %v", err)
	}
	if got := len(s.received()); got != 0 {
		t.Errorf("message was delivered anyway (%d received)", got)
	}
}

func TestSendUsesSTARTTLS(t *testing.T) {
	cert := selfSigned(t)
	s := newFakeServer(t, func(s *fakeServer) {
		s.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}}
	})

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	cfg := s.config()
	cfg.TLSConfig = &tls.Config{RootCAs: pool}
	cfg.Username, cfg.Password = "user", "secret"

	if err := NewSMTP(cfg).Send(t.Context(), &Message{
		To: []string{"a@example.com"}, Text: "hi",
	}); err != nil {
		t.Fatalf("Send over STARTTLS: %v", err)
	}
	if got := len(s.received()); got != 1 {
		t.Fatalf("server received %d messages, want 1", got)
	}
	if _, auths := s.counts(); auths != 1 {
		t.Errorf("server saw %d AUTH commands, want 1", auths)
	}
}

func TestSendTimesOutOnAStalledServer(t *testing.T) {
	// The regression this guards: a dial timeout alone does not cover a server
	// that completes the handshake and then goes quiet.
	s := newFakeServer(t, func(s *fakeServer) { s.stall = true })
	cfg := s.config()
	cfg.Timeout = 200 * time.Millisecond
	sender := NewSMTP(cfg)

	start := time.Now()
	err := sender.Send(t.Context(), &Message{To: []string{"a@example.com"}, Text: "hi"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a stalled server did not produce an error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v to give up, want roughly the 200ms idle timeout", elapsed)
	}
}

func TestSendHonoursContextCancellation(t *testing.T) {
	s := newFakeServer(t, func(s *fakeServer) { s.stall = true })
	cfg := s.config()
	cfg.Timeout = 30 * time.Second // long, so only the context can end this
	sender := NewSMTP(cfg)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sender.Send(ctx, &Message{To: []string{"a@example.com"}, Text: "hi"})
	if err == nil {
		t.Fatal("cancelled send reported success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v to cancel", elapsed)
	}
}

func TestSendReportsDialFailure(t *testing.T) {
	// Bind and immediately release a port, so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()

	sender := NewSMTP(Config{
		Host: "127.0.0.1", Port: port, From: "s@example.com",
		Timeout: time.Second, AllowInsecure: true,
	})
	if err := sender.Send(t.Context(), &Message{
		To: []string{"a@example.com"}, Text: "hi",
	}); err == nil {
		t.Fatal("Send to a dead port reported success")
	}
}

func TestSendManyWithNoMessages(t *testing.T) {
	s := newFakeServer(t, nil)
	if errs := NewSMTP(s.config()).SendMany(t.Context(), nil); len(errs) != 0 {
		t.Errorf("SendMany(nil) = %v, want an empty slice", errs)
	}
	if conns, _ := s.counts(); conns != 0 {
		t.Errorf("empty batch opened %d connections, want 0", conns)
	}
}

// selfSigned generates a throwaway certificate valid for 127.0.0.1.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
