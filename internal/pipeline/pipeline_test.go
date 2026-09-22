package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type recorder struct{ events []string }

func (r *recorder) step(name string, runErr error, withCleanup bool, cleanupErr error) Step {
	return Step{Name: name, Run: func(ctx context.Context) (Cleanup, error) {
		r.events = append(r.events, "run "+name)
		if !withCleanup {
			return nil, runErr
		}
		return func(ctx context.Context) error {
			r.events = append(r.events, "undo "+name)
			return cleanupErr
		}, runErr
	}}
}

func TestRunExecutesStepsInOrder(t *testing.T) {
	r := &recorder{}
	p := &Pipeline{}
	err := p.Run(context.Background(), []Step{r.step("a", nil, true, nil), r.step("b", nil, false, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"run a", "run b"}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestRunStopsAtFailingStepAndWrapsError(t *testing.T) {
	boom := errors.New("boom")
	r := &recorder{}
	p := &Pipeline{}
	err := p.Run(context.Background(), []Step{r.step("a", nil, true, nil), r.step("b", boom, false, nil), r.step("c", nil, false, nil)})
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "b: ") {
		t.Fatalf("err = %v, want wrapped boom prefixed with step name", err)
	}
	if want := []string{"run a", "run b"}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestCleanupRunsInReverseIncludingFailingStep(t *testing.T) {
	r := &recorder{}
	p := &Pipeline{}
	_ = p.Run(context.Background(), []Step{
		r.step("a", nil, true, nil),
		r.step("b", nil, false, nil),
		r.step("c", errors.New("half done"), true, nil),
	})
	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"run a", "run b", "run c", "undo c", "undo a"}
	if !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestCleanupContinuesPastErrorsAndJoinsThem(t *testing.T) {
	e1, e2 := errors.New("e1"), errors.New("e2")
	r := &recorder{}
	p := &Pipeline{}
	_ = p.Run(context.Background(), []Step{r.step("a", nil, true, e1), r.step("b", nil, true, e2)})
	err := p.Cleanup(context.Background())
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("err = %v, want both e1 and e2", err)
	}
	if want := []string{"run a", "run b", "undo b", "undo a"}; !slices.Equal(r.events, want) {
		t.Fatalf("events = %v, want %v", r.events, want)
	}
}

func TestRunStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &recorder{}
	cancelling := Step{Name: "cancel", Run: func(context.Context) (Cleanup, error) {
		r.events = append(r.events, "run cancel")
		cancel()
		return nil, nil
	}}
	p := &Pipeline{}
	err := p.Run(ctx, []Step{cancelling, r.step("never", nil, false, nil)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if slices.Contains(r.events, "run never") {
		t.Fatal("step after cancellation ran")
	}
}

func TestProgressIsReported(t *testing.T) {
	var lines []string
	p := &Pipeline{Progress: func(f string, a ...any) { lines = append(lines, f) }}
	r := &recorder{}
	_ = p.Run(context.Background(), []Step{r.step("a", nil, true, nil)})
	_ = p.Cleanup(context.Background())
	if len(lines) != 2 {
		t.Fatalf("progress lines = %v, want 2", lines)
	}
}
