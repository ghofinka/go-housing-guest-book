package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/config"
)

const (
	smtpDialTimeout = 10 * time.Second
	// smtpIOTimeout bounds one whole delivery so a stalled server cannot hold the worker.
	smtpIOTimeout = 30 * time.Second
)

// SMTPSender sends mail through an SMTP server.
//
// Port 465 uses TLS from the first byte. Any other port must offer STARTTLS when a login is
// configured: credentials are never sent over a plaintext connection. With no login (an internal
// relay) a server without STARTTLS is accepted.
type SMTPSender struct {
	cfg config.SMTP
	// tlsConfig is nil in production (the default verifies the server's name); tests inject one
	// that trusts their self-signed certificate.
	tlsConfig *tls.Config
	// forceImplicitTLS lets tests use TLS from the first byte on a port other than 465.
	forceImplicitTLS bool
}

func NewSMTPSender(cfg config.SMTP) *SMTPSender { return &SMTPSender{cfg: cfg} }

func (s *SMTPSender) tls() *tls.Config {
	if s.tlsConfig != nil {
		return s.tlsConfig
	}
	return &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
}

// Send delivers one message. Its error never contains the password; it may contain the server's
// reply, so callers must not store or log the text (see errorSummary).
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("smtp: sender address: %w", err)
	}
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("smtp: refusing a header with a line break")
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("smtp: recipient address: %w", err)
	}
	payload, err := buildMessage(from, to, m, time.Now())
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: smtpDialTimeout}
	implicitTLS := s.cfg.Port == 465 || s.forceImplicitTLS
	var conn net.Conn
	if implicitTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: s.tls()}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: connect: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(smtpIOTimeout))
	// Closing the connection is how a cancelled context interrupts a blocked read or write.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: greeting: %w", err)
	}
	defer c.Close()

	if !implicitTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.tls()); err != nil {
				return fmt.Errorf("smtp: starttls: %w", err)
			}
		} else if s.cfg.Username != "" {
			return errors.New("smtp: server does not offer STARTTLS; refusing to send credentials in plaintext")
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: mail from: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("smtp: rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: end of data: %w", err)
	}
	return c.Quit()
}

// buildMessage renders an RFC 5322 message: UTF-8 text, quoted-printable, with an encoded subject.
// Every header value comes from a trusted source (parsed addresses and a fixed subject).
func buildMessage(from, to *mail.Address, m Message, now time.Time) ([]byte, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("smtp: message id: %w", err)
	}
	domain := "localhost"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 && i+1 < len(from.Address) {
		domain = from.Address[i+1:]
	}

	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	h("To", to.String())
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id[:])+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=UTF-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n"))); err != nil {
		return nil, fmt.Errorf("smtp: encode body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("smtp: encode body: %w", err)
	}
	return []byte(b.String()), nil
}
