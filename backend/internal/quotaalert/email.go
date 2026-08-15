package quotaalert

import (
	"context"
	"fmt"
	"html"
	"strings"
)

type EmailSender interface {
	SendEmail(ctx context.Context, to, subject, body string) error
}

type EmailSink struct {
	sender EmailSender
}

func NewEmailSink(sender EmailSender) *EmailSink {
	return &EmailSink{sender: sender}
}

func (s *EmailSink) Name() string {
	return "email"
}

func (s *EmailSink) Deliver(ctx context.Context, event Event) error {
	if s == nil || s.sender == nil {
		return nil
	}
	recipient := strings.TrimSpace(event.Recipient)
	if recipient == "" {
		return nil
	}
	subject := fmt.Sprintf(
		"Quota alert: %s usage reached %.0f%%",
		event.GroupName,
		event.Threshold,
	)
	body := fmt.Sprintf(
		"<h2>Quota usage alert</h2>"+
			"<p>Group: %s</p><p>Window: %s</p>"+
			"<p>Usage: %.2f / %.2f USD (%.2f%%)</p>"+
			"<p>Threshold: %.0f%%</p><p>Resets at: %s</p>",
		html.EscapeString(event.GroupName),
		html.EscapeString(event.Window),
		event.UsedUSD,
		event.LimitUSD,
		event.Percentage,
		event.Threshold,
		event.ResetsAt.UTC().Format("2006-01-02 15:04:05 UTC"),
	)
	return s.sender.SendEmail(ctx, recipient, subject, body)
}
