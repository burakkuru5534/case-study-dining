package ingestion

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queueProcessor hands out `items` events; each takes `work` to process.
type queueProcessor struct {
	mu        sync.Mutex
	items     int
	work      time.Duration
	inFlight  int
	maxFlight int
	calls     atomic.Int64
	done      atomic.Int64
}

func (q *queueProcessor) ProcessNext(ctx context.Context) (bool, error) {
	q.calls.Add(1)
	q.mu.Lock()
	if q.items == 0 {
		q.mu.Unlock()
		return false, nil
	}
	q.items--
	q.inFlight++
	q.maxFlight = max(q.maxFlight, q.inFlight)
	q.mu.Unlock()

	time.Sleep(q.work)

	q.mu.Lock()
	q.inFlight--
	q.mu.Unlock()
	q.done.Add(1)
	return true, nil
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func runFor(w *Worker, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	w.Run(ctx)
}

func TestWorker_DrainsQueueWithBoundedConcurrency(t *testing.T) {
	t.Parallel()
	q := &queueProcessor{items: 30, work: 5 * time.Millisecond}

	runFor(NewWorker(q, WorkerConfig{Concurrency: 3, PollInterval: 20 * time.Millisecond}, quiet()), time.Second)

	if q.done.Load() != 30 {
		t.Fatalf("processed %d, want 30", q.done.Load())
	}
	if q.maxFlight != 3 {
		t.Fatalf("max concurrent = %d, want exactly 3", q.maxFlight)
	}
}

func TestWorker_IdlePollingUsesInterval(t *testing.T) {
	t.Parallel()
	q := &queueProcessor{}

	runFor(NewWorker(q, WorkerConfig{Concurrency: 2, PollInterval: 50 * time.Millisecond}, quiet()), 260*time.Millisecond)

	// 2 loops × ~6 polls in 260ms. The bounds only need to rule out a busy
	// loop (thousands of calls) and a stalled one; timing on slow CI varies.
	if c := q.calls.Load(); c < 2 || c > 20 {
		t.Fatalf("calls = %d, want roughly 12", c)
	}
}

// blockingProcessor blocks inside ProcessNext until released.
type blockingProcessor struct {
	entered chan struct{}
	release chan struct{}
	ctxErr  atomic.Value // error seen by the in-flight call when released
	calls   atomic.Int64
}

func (b *blockingProcessor) ProcessNext(ctx context.Context) (bool, error) {
	if b.calls.Add(1) > 1 {
		return false, nil
	}
	close(b.entered)
	<-b.release
	b.ctxErr.Store(ctxErrOrNil(ctx))
	return true, nil
}

func ctxErrOrNil(ctx context.Context) any {
	if err := ctx.Err(); err != nil {
		return err
	}
	return "nil"
}

func TestWorker_ShutdownFinishesInFlightEvent(t *testing.T) {
	t.Parallel()
	b := &blockingProcessor{entered: make(chan struct{}), release: make(chan struct{})}
	w := NewWorker(b, WorkerConfig{Concurrency: 1, PollInterval: 10 * time.Millisecond}, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()

	<-b.entered
	cancel() // SIGTERM while an event is being processed

	select {
	case <-stopped:
		t.Fatal("Run returned while an event was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(b.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the in-flight event finished")
	}

	if got := b.ctxErr.Load(); got != "nil" {
		t.Fatalf("in-flight event's context was cancelled by shutdown: %v", got)
	}
	if c := b.calls.Load(); c != 1 {
		t.Fatalf("calls = %d: a new event was claimed after shutdown began", c)
	}
}

// failingProcessor always fails, recording call times.
type failingProcessor struct {
	mu    sync.Mutex
	times []time.Time
}

func (f *failingProcessor) ProcessNext(context.Context) (bool, error) {
	f.mu.Lock()
	f.times = append(f.times, time.Now())
	f.mu.Unlock()
	return false, errors.New("connection refused")
}

func TestWorker_ErrorsBackOffExponentiallyWithCap(t *testing.T) {
	t.Parallel()
	f := &failingProcessor{}

	runFor(NewWorker(f, WorkerConfig{Concurrency: 1, PollInterval: 20 * time.Millisecond, MaxErrorWait: 80 * time.Millisecond}, quiet()),
		400*time.Millisecond)

	f.mu.Lock()
	defer f.mu.Unlock()
	// Waits: 20, 40, 80, 80 ... → about 6 calls in 400ms; a hot loop would be thousands.
	if n := len(f.times); n < 3 || n > 10 {
		t.Fatalf("calls = %d, want about 6", n)
	}
	// Each wait is at least the expected backoff (strict), and the cap holds
	// (loose upper bound, so a slow machine does not make this flaky).
	for i, want := range []time.Duration{20, 40, 80, 80} {
		if i+1 >= len(f.times) {
			break
		}
		gap := f.times[i+1].Sub(f.times[i])
		if lo := want * time.Millisecond; gap < lo {
			t.Errorf("gap %d = %s, shorter than backoff %s", i, gap, lo)
		}
		if gap > 80*time.Millisecond+150*time.Millisecond {
			t.Errorf("gap %d = %s, exceeds MaxErrorWait by far", i, gap)
		}
	}
}

// deadlineProcessor reports the deadline it was given and waits for it.
type deadlineProcessor struct {
	once     sync.Once
	deadline chan time.Duration
}

func (d *deadlineProcessor) ProcessNext(ctx context.Context) (bool, error) {
	dl, _ := ctx.Deadline()
	d.once.Do(func() { d.deadline <- time.Until(dl) })
	<-ctx.Done()
	return true, ctx.Err()
}

func TestWorker_EventTimeoutBoundsOneEvent(t *testing.T) {
	t.Parallel()
	d := &deadlineProcessor{deadline: make(chan time.Duration, 1)}
	w := NewWorker(d, WorkerConfig{Concurrency: 1, EventTimeout: 50 * time.Millisecond, PollInterval: 10 * time.Millisecond}, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()

	if got := <-d.deadline; got <= 0 || got > 50*time.Millisecond {
		t.Fatalf("event deadline = %s, want ≤ 50ms", got)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("a stuck event blocked shutdown beyond EventTimeout")
	}
}
