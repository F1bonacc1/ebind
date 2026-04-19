package task

import (
	"encoding/json"
	"time"
)

type Task struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Payload     json.RawMessage   `json:"payload"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Deadline    time.Time         `json:"deadline,omitempty"`
	EnqueuedAt  time.Time         `json:"enqueued_at"`
	Attempt     int               `json:"attempt"`
	TraceCtx    map[string]string `json:"trace_ctx,omitempty"`
	DAGID       string            `json:"dag_id,omitempty"`
	StepID      string            `json:"step_id,omitempty"`
	RetryPolicy *RetryPolicy      `json:"retry_policy,omitempty"`
	Target      string            `json:"target,omitempty"`
	WorkerID    string            `json:"worker_id,omitempty"`
}

type Response struct {
	TaskID      string          `json:"task_id"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *TaskError      `json:"error,omitempty"`
	Attempts    int             `json:"attempts"`
	CompletedAt time.Time       `json:"completed_at"`
}

type TaskError struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *TaskError) Error() string { return e.Kind + ": " + e.Message }

const (
	ErrKindHandler        = "handler"
	ErrKindDecode         = "decode"
	ErrKindUnknownHandler = "unknown_handler"
	ErrKindPanic          = "panic"
	ErrKindDeadline       = "deadline"
	ErrKindArgCount       = "arg_count"
	ErrKindArgType        = "arg_type"
)
