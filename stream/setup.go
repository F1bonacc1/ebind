// Package stream creates and manages ebind's JetStream streams.
package stream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	TaskStream     = "EBIND_TASKS"
	ResponseStream = "EBIND_RESP"
	DLQStream      = "EBIND_DLQ"

	TaskSubjectPrefix     = "TASKS."
	ResponseSubjectPrefix = "RESP."
	DLQSubjectPrefix      = "DLQ."
)

type Config struct {
	Replicas        int           // 1 for dev, 3 for HA
	TaskMaxAge      time.Duration // default 24h
	ResponseMaxAge  time.Duration // default 1h
	DLQMaxAge       time.Duration // default 7d
	DuplicateWindow time.Duration // default 5m
}

func (c *Config) setDefaults() {
	if c.Replicas == 0 {
		c.Replicas = 1
	}
	if c.TaskMaxAge == 0 {
		c.TaskMaxAge = 24 * time.Hour
	}
	if c.ResponseMaxAge == 0 {
		c.ResponseMaxAge = time.Hour
	}
	if c.DLQMaxAge == 0 {
		c.DLQMaxAge = 7 * 24 * time.Hour
	}
	if c.DuplicateWindow == 0 {
		c.DuplicateWindow = 5 * time.Minute
	}
}

// EnsureStreams creates or updates all three ebind streams idempotently.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, cfg Config) error {
	cfg.setDefaults()

	tasksCfg := jetstream.StreamConfig{
		Name:       TaskStream,
		Subjects:   []string{TaskSubjectPrefix + ">"},
		Retention:  jetstream.WorkQueuePolicy,
		Storage:    jetstream.FileStorage,
		Replicas:   cfg.Replicas,
		MaxAge:     cfg.TaskMaxAge,
		Duplicates: cfg.DuplicateWindow,
	}
	if _, err := js.CreateOrUpdateStream(ctx, tasksCfg); err != nil {
		return fmt.Errorf("stream: tasks: %w", err)
	}

	respCfg := jetstream.StreamConfig{
		Name:      ResponseStream,
		Subjects:  []string{ResponseSubjectPrefix + ">"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		Replicas:  cfg.Replicas,
		MaxAge:    cfg.ResponseMaxAge,
	}
	if _, err := js.CreateOrUpdateStream(ctx, respCfg); err != nil {
		return fmt.Errorf("stream: responses: %w", err)
	}

	dlqCfg := jetstream.StreamConfig{
		Name:      DLQStream,
		Subjects:  []string{DLQSubjectPrefix + ">"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		Replicas:  cfg.Replicas,
		MaxAge:    cfg.DLQMaxAge,
	}
	if _, err := js.CreateOrUpdateStream(ctx, dlqCfg); err != nil {
		return fmt.Errorf("stream: dlq: %w", err)
	}

	return nil
}

// WaitMetaLeader polls until the JetStream meta-leader is elected or ctx expires.
// Needed on cluster startup before any stream operation.
func WaitMetaLeader(ctx context.Context, js jetstream.JetStream) error {
	for {
		_, err := js.AccountInfo(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
