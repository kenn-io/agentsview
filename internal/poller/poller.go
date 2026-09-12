// Package poller runs interval-driven background jobs.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"
)

// Job is a startup definition with a unique Name and positive Interval.
type Job struct {
	Name       string
	Interval   time.Duration
	Jitter     time.Duration
	Cooldown   time.Duration
	RunAtStart bool
	// Run must return promptly when its context is canceled.
	Run func(context.Context) error
}

// RetryAfterError overrides the next attempt's backoff and jitter.
type RetryAfterError struct {
	RetryAfter time.Duration
	Err        error
}

func (e *RetryAfterError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("retry after %s: %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("retry after %s", e.RetryAfter)
}

func (e *RetryAfterError) Unwrap() error { return e.Err }

type Status struct {
	Name                string    `json:"name"`
	LastAttempt         time.Time `json:"last_attempt,omitzero"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NextRun             time.Time `json:"next_run,omitzero"`
}

type job struct {
	Job
	trigger chan chan error
	mu      sync.Mutex
	status  Status
}

// Scheduler owns a fixed set of jobs and their process-local status.
type Scheduler struct {
	ctx  context.Context
	jobs []*job
	wg   sync.WaitGroup
}

// Start launches the supplied jobs. Cancel ctx, then Wait to stop them.
func Start(ctx context.Context, jobs ...Job) *Scheduler {
	s := &Scheduler{ctx: ctx}
	for _, definition := range jobs {
		j := &job{
			Job:     definition,
			trigger: make(chan chan error, 1),
			status:  Status{Name: definition.Name},
		}
		s.jobs = append(s.jobs, j)
		s.wg.Go(func() { s.run(j) })
	}
	return s
}

func (s *Scheduler) Wait() { s.wg.Wait() }

// Status returns snapshots in startup order.
func (s *Scheduler) Status() []Status {
	out := make([]Status, len(s.jobs))
	for i, j := range s.jobs {
		j.mu.Lock()
		out[i] = j.status
		j.mu.Unlock()
	}
	return out
}

var ErrUnknownJob = errors.New("poller: unknown job")

// TriggerNow bypasses timing limits and waits for a serialized attempt.
// One trigger may queue behind a running attempt; shutdown cancels both.
func (s *Scheduler) TriggerNow(name string) error {
	for _, j := range s.jobs {
		if j.Name != name {
			continue
		}
		reply := make(chan error, 1)
		select {
		case j.trigger <- reply:
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
			return fmt.Errorf("poller: %q already has a trigger pending", name)
		}
		select {
		case err := <-reply:
			return err
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	return ErrUnknownJob
}

func (s *Scheduler) run(j *job) {
	status := Status{Name: j.Name, NextRun: time.Now()}
	if !j.RunAtStart {
		status.NextRun = status.NextRun.Add(j.Interval + jitter(j.Jitter))
	}
	j.publish(status)
	timer := time.NewTimer(time.Until(status.NextRun))
	defer timer.Stop()

	for {
		var reply chan error
		select {
		case <-s.ctx.Done():
			return
		case reply = <-j.trigger:
		case <-timer.C:
		}
		if s.ctx.Err() != nil {
			return
		}

		var err error
		now := time.Now()
		cooldown := status.LastAttempt.Add(j.Cooldown)
		if reply == nil && now.Before(cooldown) {
			status.NextRun = cooldown.Add(jitter(j.Jitter))
		} else {
			status.LastAttempt = now
			j.publish(status)
			err = j.Run(s.ctx)
			now = time.Now()
			delay := j.Interval
			if err == nil {
				status.LastSuccess = now
				status.LastError = ""
				status.ConsecutiveFailures = 0
			} else {
				status.LastError = err.Error()
				status.ConsecutiveFailures++
				delay = backoff(j.Interval, status.ConsecutiveFailures)
				if s.ctx.Err() == nil {
					log.Printf("poller: %s: %v", j.Name, err)
				}
			}
			if retry, ok := errors.AsType[*RetryAfterError](err); ok {
				status.NextRun = now.Add(retry.RetryAfter)
			} else {
				status.NextRun = now.Add(delay + jitter(j.Jitter))
			}
		}
		j.publish(status)
		timer.Reset(time.Until(status.NextRun))
		if reply != nil {
			reply <- err
		}
	}
}

func (j *job) publish(status Status) {
	j.mu.Lock()
	j.status = status
	j.mu.Unlock()
}

func jitter(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(limit)))
}

// Failure delays double from interval/16, reaching interval on the fifth failure.
func backoff(interval time.Duration, failures int) time.Duration {
	if failures >= 5 || interval < 16*time.Nanosecond {
		return interval
	}
	return (interval / 16) << (failures - 1)
}
