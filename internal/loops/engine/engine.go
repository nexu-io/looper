// Package engine owns the role-independent lifecycle algorithm. Roles describe
// their ordered steps and execute one step; the engine owns checkpoint progression,
// failure classification, blocking, and the single-owner lease.
package engine

import (
	"context"
	"errors"
	"fmt"
)

var ErrLeaseHeld = errors.New("loop lifecycle lease is held by another reconciler")

type Boundary string

type StepResult[C any] struct {
	Checkpoint C
	Blocked    *Blocked
}

type Blocked struct {
	Condition string
	Reason    string
}

type Failure struct {
	Class     string
	Boundary  Boundary
	Retryable bool
	Err       error
}

func (f *Failure) Error() string {
	if f == nil || f.Err == nil {
		return "lifecycle step failed"
	}
	return f.Err.Error()
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

type Plugin[C any] interface {
	Steps() []string
	ExecuteStep(context.Context, string, C) (StepResult[C], error)
	BoundaryFor(string) Boundary
	Classify(error, Boundary) *Failure
}

type Store[C any] interface {
	Load(context.Context) (checkpoint C, lastCompleted string, err error)
	StepStarted(context.Context, string, C) error
	StepCompleted(context.Context, string, C) error
	Blocked(context.Context, string, C, Blocked) error
	Done(context.Context, C) error
}

type Lease interface {
	Acquire(context.Context) (bool, error)
	Release(context.Context) error
}

type Engine[C any] struct {
	Plugin Plugin[C]
	Store  Store[C]
	Lease  Lease
}

func (e Engine[C]) Run(ctx context.Context) (C, error) {
	var zero C
	if e.Plugin == nil || e.Store == nil {
		return zero, fmt.Errorf("lifecycle engine requires plugin and store")
	}
	if e.Lease != nil {
		acquired, err := e.Lease.Acquire(ctx)
		if err != nil {
			return zero, err
		}
		if !acquired {
			return zero, ErrLeaseHeld
		}
		defer func() { _ = e.Lease.Release(context.Background()) }()
	}
	checkpoint, lastCompleted, err := e.Store.Load(ctx)
	if err != nil {
		return zero, err
	}
	steps := e.Plugin.Steps()
	start := 0
	if lastCompleted != "" {
		for i, step := range steps {
			if step == lastCompleted {
				start = i + 1
				break
			}
		}
	}
	for _, step := range steps[start:] {
		if err := ctx.Err(); err != nil {
			return checkpoint, err
		}
		if err := e.Store.StepStarted(ctx, step, checkpoint); err != nil {
			return checkpoint, err
		}
		result, stepErr := e.Plugin.ExecuteStep(ctx, step, checkpoint)
		if stepErr != nil {
			failure := e.Plugin.Classify(stepErr, e.Plugin.BoundaryFor(step))
			if failure == nil {
				failure = &Failure{Class: "unknown", Boundary: e.Plugin.BoundaryFor(step), Err: stepErr}
			}
			return checkpoint, failure
		}
		checkpoint = result.Checkpoint
		if result.Blocked != nil {
			if err := e.Store.Blocked(ctx, step, checkpoint, *result.Blocked); err != nil {
				return checkpoint, err
			}
			return checkpoint, nil
		}
		if err := e.Store.StepCompleted(ctx, step, checkpoint); err != nil {
			return checkpoint, err
		}
	}
	if err := e.Store.Done(ctx, checkpoint); err != nil {
		return checkpoint, err
	}
	return checkpoint, nil
}
