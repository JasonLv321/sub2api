package quotaalert

import (
	"context"
	"strings"
	"testing"
)

type emailSenderStub struct {
	to      string
	subject string
	body    string
}

func (s *emailSenderStub) SendEmail(
	_ context.Context,
	to string,
	subject string,
	body string,
) error {
	s.to = to
	s.subject = subject
	s.body = body
	return nil
}

func TestEmailSinkRendersAndSendsDirectly(t *testing.T) {
	sender := &emailSenderStub{}
	sink := NewEmailSink(sender)
	event := Event{
		Recipient:  "user@example.com",
		GroupName:  "<internal>",
		Window:     WindowMonthly,
		Threshold:  80,
		Percentage: 82,
		UsedUSD:    82,
		LimitUSD:   100,
	}

	if err := sink.Deliver(context.Background(), event); err != nil {
		t.Fatalf("deliver email: %v", err)
	}
	if sender.to != event.Recipient {
		t.Fatalf("recipient = %q, want %q", sender.to, event.Recipient)
	}
	if !strings.Contains(sender.subject, "80%") {
		t.Fatalf("subject does not contain threshold: %q", sender.subject)
	}
	if strings.Contains(sender.body, "<internal>") {
		t.Fatal("email body contains unescaped group name")
	}
}
