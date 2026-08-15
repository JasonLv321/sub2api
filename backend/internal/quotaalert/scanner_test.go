package quotaalert

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type scannerSourceStub struct {
	mu       sync.Mutex
	pages    map[int][]Subscription
	progress map[int64]*Progress
	calls    []int
}

func (s *scannerSourceStub) ListActive(
	_ context.Context,
	page int,
	_ int,
) ([]Subscription, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, page)
	return s.pages[page], len(s.pages), nil
}

func (s *scannerSourceStub) GetProgress(
	_ context.Context,
	id int64,
) (*Progress, error) {
	progress, ok := s.progress[id]
	if !ok {
		return nil, errors.New("progress not found")
	}
	return progress, nil
}

type deliveryStoreStub struct {
	mu   sync.Mutex
	sent map[string]bool
}

func (s *deliveryStoreStub) Exists(
	_ context.Context,
	key string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[key], nil
}

func (s *deliveryStoreStub) Mark(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[key] = true
	return nil
}

type sinkStub struct {
	mu          sync.Mutex
	name        string
	deliveryErr error
	events      []Event
}

func (s *sinkStub) Name() string {
	return s.name
}

func (s *sinkStub) Deliver(_ context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return s.deliveryErr
}

func TestScannerThresholdsIdempotencyAndWindowRollover(t *testing.T) {
	firstWindow := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	source := &scannerSourceStub{
		pages: map[int][]Subscription{
			1: {{ID: 11, UserID: 7, Email: "user@example.com"}},
		},
		progress: map[int64]*Progress{
			11: {
				GroupName: "Anthropic 100",
				Monthly:   windowProgress(85, firstWindow),
			},
		},
	}
	store := &deliveryStoreStub{sent: make(map[string]bool)}
	sink := &sinkStub{name: "test"}
	scanner := NewScanner(Config{
		Enabled:    true,
		BatchSize:  100,
		Thresholds: []float64{80, 100},
	}, source, store, nil, []Sink{sink})

	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if got := len(sink.events); got != 1 {
		t.Fatalf("80%% scan events = %d, want 1", got)
	}
	if sink.events[0].Threshold != 80 {
		t.Fatalf("threshold = %v, want 80", sink.events[0].Threshold)
	}

	source.progress[11].Monthly.Percentage = 100
	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("100%% scan: %v", err)
	}
	if got := len(sink.events); got != 2 {
		t.Fatalf("100%% scan events = %d, want 2", got)
	}
	if sink.events[1].Threshold != 100 {
		t.Fatalf("threshold = %v, want 100", sink.events[1].Threshold)
	}

	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("repeat scan: %v", err)
	}
	if got := len(sink.events); got != 2 {
		t.Fatalf("repeat scan events = %d, want 2", got)
	}

	secondWindow := firstWindow.Add(30 * 24 * time.Hour)
	source.progress[11].Monthly = windowProgress(85, secondWindow)
	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("new window scan: %v", err)
	}
	if got := len(sink.events); got != 3 {
		t.Fatalf("new window events = %d, want 3", got)
	}
	if !sink.events[2].WindowStart.Equal(secondWindow) {
		t.Fatalf("window start = %v, want %v", sink.events[2].WindowStart,
			secondWindow)
	}
}

func TestScannerUsesExplicitPagination(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	source := &scannerSourceStub{
		pages: map[int][]Subscription{
			1: {{ID: 1}},
			2: {{ID: 2}},
			3: {{ID: 3}},
		},
		progress: map[int64]*Progress{
			1: {Monthly: windowProgress(0, start)},
			2: {Monthly: windowProgress(0, start)},
			3: {Monthly: windowProgress(0, start)},
		},
	}
	scanner := NewScanner(Config{
		Enabled: true, BatchSize: 1, Thresholds: []float64{80},
	}, source, &deliveryStoreStub{sent: make(map[string]bool)}, nil, nil)

	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := len(source.calls); got != 3 {
		t.Fatalf("page calls = %d, want 3", got)
	}
}

func TestScannerDisabledHasNoSideEffects(t *testing.T) {
	source := &scannerSourceStub{}
	store := &deliveryStoreStub{sent: make(map[string]bool)}
	sink := &sinkStub{name: "test"}
	scanner := NewScanner(Config{
		Enabled: false, Interval: time.Millisecond, BatchSize: 10,
	}, source, store, nil, []Sink{sink})

	scanner.Start()
	scanner.Stop()

	if len(source.calls) != 0 {
		t.Fatalf("disabled scanner made %d list calls", len(source.calls))
	}
	if len(store.sent) != 0 || len(sink.events) != 0 {
		t.Fatal("disabled scanner produced a side effect")
	}
}

func TestScannerIsolatesSinkFailuresAndRetriesOnlyFailedSink(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	source := &scannerSourceStub{
		pages: map[int][]Subscription{
			1: {{ID: 11, UserID: 7, Email: "user@example.com"}},
		},
		progress: map[int64]*Progress{
			11: {
				GroupName: "Internal",
				Monthly:   windowProgress(85, start),
			},
		},
	}
	store := &deliveryStoreStub{sent: make(map[string]bool)}
	failing := &sinkStub{
		name: "email", deliveryErr: errors.New("email unavailable"),
	}
	successful := &sinkStub{name: "webhook"}
	scanner := NewScanner(Config{
		Enabled: true, BatchSize: 100, Thresholds: []float64{80},
	}, source, store, nil, []Sink{failing, successful})

	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if got := len(failing.events); got != 1 {
		t.Fatalf("failing sink calls = %d, want 1", got)
	}
	if got := len(successful.events); got != 1 {
		t.Fatalf("successful sink calls = %d, want 1", got)
	}
	if got := len(store.sent); got != 1 {
		t.Fatalf("delivery keys = %d, want 1", got)
	}
	event := successful.events[0]
	if store.sent[deliveryKey("email", event)] {
		t.Fatal("failing sink wrote its delivery key")
	}
	if !store.sent[deliveryKey("webhook", event)] {
		t.Fatal("successful sink did not write its delivery key")
	}

	if err := scanner.scanOnce(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if got := len(failing.events); got != 2 {
		t.Fatalf("failing sink calls = %d, want 2", got)
	}
	if got := len(successful.events); got != 1 {
		t.Fatalf("successful sink calls = %d, want 1", got)
	}
	if got := len(store.sent); got != 1 {
		t.Fatalf("delivery keys = %d, want 1", got)
	}
}

func windowProgress(percentage float64, start time.Time) *WindowProgress {
	return &WindowProgress{
		LimitUSD:    100,
		UsedUSD:     percentage,
		Percentage:  percentage,
		WindowStart: start,
		ResetsAt:    start.Add(30 * 24 * time.Hour),
	}
}
