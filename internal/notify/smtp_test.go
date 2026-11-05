package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/config"
)

// fakeSMTP is a small in-process SMTP server that records what a client sends. It can offer STARTTLS,
// speak TLS from the first byte, and refuse the recipient, so the sender's rules about credentials and
// error handling can be checked without a real mail server.
type fakeSMTP struct {
	ln          net.Listener
	tlsCfg      *tls.Config
	startTLS    bool   // offer STARTTLS
	implicit    bool   // TLS from the first byte
	rcptReply   string // reply to RCPT TO, default "250 ok"
	authAnytime bool   // advertise AUTH even before TLS (to prove the client refuses anyway)

	mu  sync.Mutex
	got struct {
		from, rcpt, data string
		auth             string // decoded AUTH PLAIN payload
		authBeforeTLS    bool
		tlsActive        bool
		sawData          bool
		sawAuth          bool
	}
	done chan struct{}
}

func newFakeSMTP(t *testing.T, f *fakeSMTP) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	f.done = make(chan struct{})
	if f.rcptReply == "" {
		f.rcptReply = "250 ok"
	}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	defer close(f.done)
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	tlsActive := false
	if f.implicit {
		conn = tls.Server(conn, f.tlsCfg)
		tlsActive = true
	}
	r := bufio.NewReader(conn)
	say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			say("250-fake")
			if f.startTLS && !tlsActive {
				say("250-STARTTLS")
			}
			if tlsActive || f.authAnytime {
				say("250-AUTH PLAIN")
			}
			say("250 8BITMIME")
		case up == "STARTTLS":
			say("220 go ahead")
			conn = tls.Server(conn, f.tlsCfg)
			r = bufio.NewReader(conn)
			say = func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
			tlsActive = true
		case strings.HasPrefix(up, "AUTH PLAIN"):
			payload, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			f.mu.Lock()
			f.got.sawAuth, f.got.auth, f.got.authBeforeTLS = true, string(payload), !tlsActive
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.got.from = line
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.got.rcpt = line
			f.mu.Unlock()
			say(f.rcptReply)
		case up == "DATA":
			f.mu.Lock()
			f.got.sawData = true
			f.mu.Unlock()
			say("354 go")
			var sb strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				sb.WriteString(l)
			}
			f.mu.Lock()
			f.got.data = sb.String()
			f.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			f.mu.Lock()
			f.got.tlsActive = tlsActive
			f.mu.Unlock()
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake SMTP server did not finish")
	}
}

// tlsFixture returns a server TLS config with a self-signed certificate and a client config that
// trusts it, borrowed from net/http/httptest so no certificate has to be generated here.
func tlsFixture(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{Certificates: srv.TLS.Certificates},
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

func sender(f *fakeSMTP, user, pass string, client *tls.Config) *SMTPSender {
	return &SMTPSender{
		cfg:       config.SMTP{Host: "127.0.0.1", Port: f.port(), Username: user, Password: pass, From: "Buku Tamu <noreply@example.test>"},
		tlsConfig: client,
	}
}

func arrival() Message {
	return arrivalMessage("owner@example.test", "kurir", time.Date(2026, 11, 5, 7, 5, 0, 0, time.UTC)) // 14:05 WIB
}

func TestSMTPSendsThroughAnInternalRelay(t *testing.T) {
	f := newFakeSMTP(t, &fakeSMTP{})
	if err := sender(f, "", "", nil).Send(context.Background(), arrival()); err != nil {
		t.Fatalf("send: %v", err)
	}
	f.wait(t)
	if !strings.Contains(f.got.from, "<noreply@example.test>") || !strings.Contains(f.got.rcpt, "<owner@example.test>") {
		t.Fatalf("envelope = %q / %q", f.got.from, f.got.rcpt)
	}
	for _, want := range []string{
		"From: ", "To: ", "Subject: Tamu tiba di rumah Anda", "MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8", "Content-Transfer-Encoding: quoted-printable",
		"Message-ID: <", "@example.test>", "Date: ",
		"Ada tamu (kurir) tiba di rumah Anda pukul 14:05 WIB.", "menyetujui atau menolak",
	} {
		if !strings.Contains(f.got.data, want) {
			t.Errorf("message lacks %q:\n%s", want, f.got.data)
		}
	}
	if f.got.sawAuth {
		t.Fatal("the client logged in although no login is configured")
	}
}

func TestSMTPRefusesToSendACredentialOverPlaintext(t *testing.T) {
	// The server even advertises AUTH on the plain connection; the client must still refuse.
	f := newFakeSMTP(t, &fakeSMTP{authAnytime: true})
	err := sender(f, "user", "pa55word", nil).Send(context.Background(), arrival())
	if err == nil || !strings.Contains(err.Error(), "refusing to send credentials in plaintext") {
		t.Fatalf("error = %v, want a refusal to send credentials in plaintext", err)
	}
	if strings.Contains(err.Error(), "pa55word") {
		t.Fatalf("the error leaks the password: %v", err)
	}
	f.wait(t)
	if f.got.sawAuth || f.got.sawData {
		t.Fatalf("credentials or mail reached a plaintext server (auth=%v data=%v)", f.got.sawAuth, f.got.sawData)
	}
}

func TestSMTPStartTLSThenLogin(t *testing.T) {
	serverTLS, clientTLS := tlsFixture(t)
	f := newFakeSMTP(t, &fakeSMTP{tlsCfg: serverTLS, startTLS: true})
	if err := sender(f, "user", "pa55word", clientTLS).Send(context.Background(), arrival()); err != nil {
		t.Fatalf("send: %v", err)
	}
	f.wait(t)
	if !f.got.tlsActive || f.got.authBeforeTLS {
		t.Fatalf("login must come after the TLS upgrade (tls=%v before=%v)", f.got.tlsActive, f.got.authBeforeTLS)
	}
	if want := "\x00user\x00pa55word"; f.got.auth != want {
		t.Fatalf("auth payload = %q, want %q", f.got.auth, want)
	}
	if !f.got.sawData {
		t.Fatal("no message was delivered")
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	serverTLS, clientTLS := tlsFixture(t)
	f := newFakeSMTP(t, &fakeSMTP{tlsCfg: serverTLS, implicit: true})
	s := sender(f, "user", "pa55word", clientTLS)
	s.forceImplicitTLS = true // production decides this by port 465
	if err := s.Send(context.Background(), arrival()); err != nil {
		t.Fatalf("send: %v", err)
	}
	f.wait(t)
	if !f.got.tlsActive || f.got.authBeforeTLS || !f.got.sawData {
		t.Fatalf("implicit TLS delivery wrong: %+v", f.got)
	}
}

func TestSMTPDoesNotTrustAnUnknownCertificate(t *testing.T) {
	serverTLS, _ := tlsFixture(t)
	f := newFakeSMTP(t, &fakeSMTP{tlsCfg: serverTLS, startTLS: true})
	// No custom root pool: the default verification must reject the self-signed certificate.
	s := &SMTPSender{cfg: config.SMTP{Host: "127.0.0.1", Port: f.port(), Username: "user", Password: "pa55word", From: "noreply@example.test"}}
	err := s.Send(context.Background(), arrival())
	if err == nil {
		t.Fatal("a self-signed certificate was accepted")
	}
	f.wait(t)
	if f.got.sawAuth || f.got.sawData {
		t.Fatal("credentials or mail were sent over a connection whose certificate was not trusted")
	}
}

func TestSMTPRefusesHeaderInjection(t *testing.T) {
	// No server is needed: the message is refused before any connection is made.
	s := &SMTPSender{cfg: config.SMTP{Host: "127.0.0.1", Port: 1, From: "noreply@example.test"}}
	for name, m := range map[string]Message{
		"line break in recipient": {To: "a@example.test\r\nBcc: x@example.test", Subject: "s", Body: "b"},
		"line break in subject":   {To: "a@example.test", Subject: "s\r\nBcc: x@example.test", Body: "b"},
		"not an address":          {To: "not an address", Subject: "s", Body: "b"},
	} {
		if err := s.Send(context.Background(), m); err == nil {
			t.Errorf("%s: the message was not refused", name)
		}
	}
}

func TestSMTPServerRefusalIsReportedWithoutTheAddress(t *testing.T) {
	f := newFakeSMTP(t, &fakeSMTP{rcptReply: "550 5.1.1 <owner@example.test>: user unknown"})
	err := sender(f, "", "", nil).Send(context.Background(), arrival())
	var perr *textproto.Error
	if !errors.As(err, &perr) || perr.Code != 550 {
		t.Fatalf("error = %v, want an SMTP 550 reply", err)
	}
	// What the worker stores and logs is the summary, never the server's text.
	if got := errorSummary(err); got != "smtp reply 550" || strings.Contains(got, "example.test") {
		t.Fatalf("errorSummary = %q, want %q", got, "smtp reply 550")
	}
	f.wait(t)
}

func TestSMTPGivesUpOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &SMTPSender{cfg: config.SMTP{Host: "127.0.0.1", Port: 9, From: "noreply@example.test"}}
	if err := s.Send(ctx, arrival()); err == nil {
		t.Fatal("a cancelled context still sent")
	}
}

func TestBuildMessageEncodesUTF8(t *testing.T) {
	m := Message{To: "o@example.test", Subject: "Tamu tiba — Bapak Budi", Body: "Selamat siang, Ibu Siti.\nÉvénement café\n"}
	from, to := mustAddr(t, "Buku Tamu <noreply@example.test>"), mustAddr(t, "o@example.test")
	raw, err := buildMessage(from, to, m, time.Date(2026, 11, 5, 7, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "Subject: =?utf-8?q?") {
		t.Errorf("a non-ASCII subject must be encoded:\n%s", s)
	}
	if !strings.Contains(s, "=C3=89v=C3=A9nement caf=C3=A9") {
		t.Errorf("the body must be quoted-printable UTF-8:\n%s", s)
	}
	if !strings.Contains(s, "\r\n\r\n") {
		t.Error("headers and body are not separated by a blank line")
	}
	for _, l := range strings.Split(s, "\r\n") {
		if strings.ContainsAny(l, "\r\n") {
			t.Fatalf("stray line break inside %q", l)
		}
	}
}

func TestArrivalMessageNeverCarriesGuestData(t *testing.T) {
	at := time.Date(2026, 11, 5, 7, 5, 0, 0, time.UTC)
	for cat, want := range map[string]string{
		"tamu": "(tamu)", "kurir": "(kurir)", "ojol": "(ojol)", "tukang": "(tukang)",
		"keluarga": "(keluarga)", "lainnya": "(tamu lain)",
		"<script>alert(1)</script>": "(tamu)", // an unknown value never reaches the email
	} {
		m := arrivalMessage("o@example.test", cat, at)
		if !strings.Contains(m.Body, want) || !strings.Contains(m.Body, "pukul 14:05 WIB") {
			t.Errorf("category %q: body = %q", cat, m.Body)
		}
		if strings.Contains(m.Body, "script") {
			t.Errorf("category %q leaked into the body: %q", cat, m.Body)
		}
	}
	// Backoff doubles from one minute.
	for attempts, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute, 0: time.Minute} {
		if got := backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func mustAddr(t *testing.T, s string) *mail.Address {
	t.Helper()
	a, err := mail.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
