package task

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

var (
	ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errType = reflect.TypeOf((*error)(nil)).Elem()
)

type Dispatcher struct {
	Name       string
	fn         reflect.Value
	argTypes   []reflect.Type
	hasResult  bool
	resultType reflect.Type
}

type Registry struct {
	mu sync.RWMutex
	m  map[string]*Dispatcher
}

func NewRegistry() *Registry {
	return &Registry{m: make(map[string]*Dispatcher)}
}

type RegisterOpt func(*regOptions)

type regOptions struct {
	name    string
	aliases []string
}

func WithName(n string) RegisterOpt {
	return func(o *regOptions) { o.name = n }
}

func Alias(n string) RegisterOpt {
	return func(o *regOptions) { o.aliases = append(o.aliases, n) }
}

func Register(r *Registry, fn any, opts ...RegisterOpt) error {
	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()

	if fnType.Kind() != reflect.Func {
		return fmt.Errorf("ebind: Register: not a function: %T", fn)
	}
	if fnType.NumIn() < 1 || fnType.In(0) != ctxType {
		return fmt.Errorf("ebind: Register: first param must be context.Context")
	}
	numOut := fnType.NumOut()
	if numOut < 1 || numOut > 2 {
		return fmt.Errorf("ebind: Register: must return (T, error) or error")
	}
	if fnType.Out(numOut-1) != errType {
		return fmt.Errorf("ebind: Register: last return must be error")
	}

	o := &regOptions{}
	for _, opt := range opts {
		opt(o)
	}

	d := &Dispatcher{fn: fnVal}
	for i := 1; i < fnType.NumIn(); i++ {
		d.argTypes = append(d.argTypes, fnType.In(i))
	}
	if numOut == 2 {
		d.hasResult = true
		d.resultType = fnType.Out(0)
	}

	name := o.name
	if name == "" {
		name = CanonicalName(fnVal)
	}
	d.Name = name

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[name]; dup {
		return fmt.Errorf("ebind: Register: duplicate handler name %q", name)
	}
	r.m[name] = d
	for _, alias := range o.aliases {
		if _, dup := r.m[alias]; dup {
			return fmt.Errorf("ebind: Register: duplicate alias %q", alias)
		}
		r.m[alias] = d
	}
	return nil
}

func MustRegister(r *Registry, fn any, opts ...RegisterOpt) {
	if err := Register(r, fn, opts...); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(name string) (*Dispatcher, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.m[name]
	return d, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	return out
}

// Call invokes the registered function. payload is a JSON array of args.
// Returns the JSON-encoded result (or nil if the handler returns only an error).
func (d *Dispatcher) Call(ctx context.Context, payload []byte) ([]byte, error) {
	var rawArgs []json.RawMessage
	if len(payload) == 0 || string(payload) == "null" {
		rawArgs = nil
	} else if err := json.Unmarshal(payload, &rawArgs); err != nil {
		return nil, &TaskError{Kind: ErrKindDecode, Message: "payload is not a JSON array: " + err.Error(), Retryable: false}
	}
	if len(rawArgs) != len(d.argTypes) {
		return nil, &TaskError{
			Kind:      ErrKindArgCount,
			Message:   fmt.Sprintf("want %d args, got %d", len(d.argTypes), len(rawArgs)),
			Retryable: false,
		}
	}

	vals := make([]reflect.Value, 0, 1+len(rawArgs))
	vals = append(vals, reflect.ValueOf(ctx))
	for i, raw := range rawArgs {
		argPtr := reflect.New(d.argTypes[i])
		if err := json.Unmarshal(raw, argPtr.Interface()); err != nil {
			return nil, &TaskError{
				Kind:      ErrKindDecode,
				Message:   fmt.Sprintf("arg %d (%s): %v", i, d.argTypes[i], err),
				Retryable: false,
			}
		}
		vals = append(vals, argPtr.Elem())
	}

	results := d.fn.Call(vals)

	var errResult error
	errVal := results[len(results)-1]
	if !errVal.IsNil() {
		errResult = errVal.Interface().(error)
	}
	if errResult != nil {
		if te, ok := errResult.(*TaskError); ok {
			return nil, te
		}
		return nil, &TaskError{Kind: ErrKindHandler, Message: errResult.Error(), Retryable: true}
	}

	if !d.hasResult {
		return nil, nil
	}
	out, err := json.Marshal(results[0].Interface())
	if err != nil {
		return nil, &TaskError{Kind: ErrKindDecode, Message: "marshal result: " + err.Error(), Retryable: false}
	}
	return out, nil
}

// ValidateArgs checks that args match the registered signature (producer-side, pre-network).
func (d *Dispatcher) ValidateArgs(args []any) error {
	if len(args) != len(d.argTypes) {
		return fmt.Errorf("arg count: want %d, got %d", len(d.argTypes), len(args))
	}
	for i, a := range args {
		want := d.argTypes[i]
		if a == nil {
			switch want.Kind() {
			case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
				continue
			default:
				return fmt.Errorf("arg %d: nil not assignable to %s", i, want)
			}
		}
		got := reflect.TypeOf(a)
		if !got.AssignableTo(want) {
			return fmt.Errorf("arg %d: %s not assignable to %s", i, got, want)
		}
	}
	return nil
}

// DescribeArgs returns the registered param types (excluding ctx). Used by Client.Enqueue.
func (d *Dispatcher) ArgTypes() []reflect.Type { return d.argTypes }

// HasResult reports whether the function returns (T, error) vs just error.
func (d *Dispatcher) HasResult() bool { return d.hasResult }

// ResultType returns T if HasResult is true.
func (d *Dispatcher) ResultType() reflect.Type { return d.resultType }

// Describe returns signature info for a function (producer-side, no registration required).
// Used by Client.Enqueue to validate args and derive the handler name.
func Describe(fn any) (*Dispatcher, error) {
	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()
	if fnType.Kind() != reflect.Func {
		return nil, fmt.Errorf("ebind: Describe: not a function: %T", fn)
	}
	if fnType.NumIn() < 1 || fnType.In(0) != ctxType {
		return nil, fmt.Errorf("ebind: Describe: first param must be context.Context")
	}
	numOut := fnType.NumOut()
	if numOut < 1 || numOut > 2 {
		return nil, fmt.Errorf("ebind: Describe: must return (T, error) or error")
	}
	if fnType.Out(numOut-1) != errType {
		return nil, fmt.Errorf("ebind: Describe: last return must be error")
	}
	d := &Dispatcher{fn: fnVal, Name: CanonicalName(fnVal)}
	for i := 1; i < fnType.NumIn(); i++ {
		d.argTypes = append(d.argTypes, fnType.In(i))
	}
	if numOut == 2 {
		d.hasResult = true
		d.resultType = fnType.Out(0)
	}
	return d, nil
}

// CanonicalName derives a stable handler name from a function value.
// Example: "github.com/you/app/handlers.Foo" → "handlers.Foo".
func CanonicalName(fnVal reflect.Value) string {
	full := runtime.FuncForPC(fnVal.Pointer()).Name()
	// Strip method receiver noise if present.
	if idx := strings.Index(full, ".func"); idx >= 0 {
		full = full[:idx]
	}
	parts := strings.Split(full, "/")
	return parts[len(parts)-1]
}
