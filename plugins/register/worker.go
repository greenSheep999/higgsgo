package register

import (
	"context"
	"log/slog"
	"time"
)

// Worker is the background registration processor.
type Worker struct {
	flow  *Flow
	store RegistrationStore
	cfg   Config
	log   *slog.Logger

	sem  chan struct{}
	done chan struct{}
}

func NewWorker(flow *Flow, store RegistrationStore, cfg Config, log *slog.Logger) *Worker {
	return &Worker{
		flow:  flow,
		store: store,
		cfg:   cfg,
		log:   log,
		sem:   make(chan struct{}, cfg.MaxConcurrent),
		done:  make(chan struct{}),
	}
}

// Start launches the background polling loop. Blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	defer close(w.done)

	for {
		select {
		case <-ctx.Done():
			w.drain()
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

// Trigger forces an immediate poll cycle outside the ticker interval.
func (w *Worker) Trigger(ctx context.Context) {
	go w.poll(ctx)
}

// Wait blocks until the worker loop has exited.
func (w *Worker) Wait() {
	<-w.done
}

func (w *Worker) poll(ctx context.Context) {
	for {
		select {
		case w.sem <- struct{}{}:
		default:
			return // at capacity
		}

		reg, err := w.store.NextPending(ctx)
		if err != nil {
			<-w.sem
			w.log.Error("failed to fetch next pending", "err", err)
			return
		}
		if reg == nil {
			<-w.sem
			return // nothing pending
		}

		go w.process(ctx, reg)
	}
}

func (w *Worker) process(ctx context.Context, reg *Registration) {
	defer func() { <-w.sem }()

	w.log.Info("starting registration", "id", reg.ID, "email", reg.Email)
	if err := w.flow.Execute(ctx, reg); err != nil {
		w.log.Error("registration failed", "id", reg.ID, "err", err)
		return
	}
	w.log.Info("registration completed", "id", reg.ID)
}

func (w *Worker) drain() {
	// Wait for all in-flight goroutines to release the semaphore.
	for i := 0; i < w.cfg.MaxConcurrent; i++ {
		w.sem <- struct{}{}
	}
}
