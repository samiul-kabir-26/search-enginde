package controlplane

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
	"time"
)

// Mailer sends transactional email.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// SMTPMailer sends plain-text mail through an SMTP relay (Mailpit in dev, without auth).
type SMTPMailer struct {
	Addr string
	From string
}

// Send implements Mailer.
func (m SMTPMailer) Send(_ context.Context, to, subject, body string) error {
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("mail: header injection rejected")
	}
	msg := "From: " + m.From + "\r\nTo: " + to + "\r\nSubject: " + subject +
		"\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + body
	if err := smtp.SendMail(m.Addr, nil, m.From, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("send mail: %w", err)
	}
	return nil
}
