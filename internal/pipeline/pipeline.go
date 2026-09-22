// Package pipeline runs ordered steps and undoes them in reverse on failure.
package pipeline

import (
	"context"
	"errors"
	"fmt"
)

// Cleanup undoes a step's effect. It receives a context independent of the
// one the step ran under, so it still works after cancellation.
type Cleanup func(ctx context.Context) error

type Step struct {
	Name string
	// Run performs the step. A non-nil Cleanup is registered even when Run
	// also returns an error (e.g. the server was created but never started).
	Run func(ctx context.Context) (Cleanup, error)
}

type Pipeline struct {
	Progress func(format string, args ...any)

	cleanups []namedCleanup
}

type namedCleanup struct {
	name string
	fn   Cleanup
}

// Run executes steps in order and stops at the first error or cancellation.
// It does not clean up; call Cleanup for that.
func (p *Pipeline) Run(ctx context.Context, steps []Step) error {
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("before %s: %w", s.Name, err)
		}
		p.progress("==> %s", s.Name)
		cleanup, err := s.Run(ctx)
		if cleanup != nil {
			p.cleanups = append(p.cleanups, namedCleanup{s.Name, cleanup})
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	return nil
}

// Cleanup runs registered cleanups in reverse order, continuing past
// failures, and returns every failure joined.
func (p *Pipeline) Cleanup(ctx context.Context) error {
	var errs []error
	for i := len(p.cleanups) - 1; i >= 0; i-- {
		c := p.cleanups[i]
		p.progress("<== undo %s", c.name)
		if err := c.fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("undo %s: %w", c.name, err))
		}
	}
	p.cleanups = nil
	return errors.Join(errs...)
}

func (p *Pipeline) progress(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(format, args...)
	}
}
