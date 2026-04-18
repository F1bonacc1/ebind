package client

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/f1bonacc1/ebind/task"
)

type Future struct {
	id string
	ch chan *task.Response
}

func (f *Future) ID() string { return f.id }

// Get blocks until the task response arrives or ctx is canceled. Unmarshals the result into out.
// Pass nil for out if the handler returns only error.
func (f *Future) Get(ctx context.Context, out any) error {
	resp, err := f.wait(ctx)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if rv := reflect.ValueOf(out); rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("ebind: Future.Get: out must be a non-nil pointer, got %T", out)
	}
	return json.Unmarshal(resp.Result, out)
}

// GetRaw returns the raw JSON-encoded result (or nil for error-only handlers).
func (f *Future) GetRaw(ctx context.Context) (json.RawMessage, error) {
	resp, err := f.wait(ctx)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (f *Future) wait(ctx context.Context) (*task.Response, error) {
	select {
	case resp, ok := <-f.ch:
		if !ok {
			return nil, ErrClientClosed
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Await is a generic helper that wraps Future.Get with a typed return.
func Await[T any](ctx context.Context, f *Future) (T, error) {
	var out T
	err := f.Get(ctx, &out)
	return out, err
}
