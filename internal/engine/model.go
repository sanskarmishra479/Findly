package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sanskarmishra479/findly/internal/embed"
)

// Model is a running embedding model. *embed.Server implements it.
type Model interface {
	EmbedPassages(ctx context.Context, title string, texts []string) ([]embed.Vector, error)
	EmbedQuery(ctx context.Context, q string) (embed.Vector, error)
	Close()
}

// StartModel starts the model. lowPriority asks for below-normal CPU priority (used
// when the model is started for indexing, so the PC stays responsive).
type StartModel func(ctx context.Context, lowPriority bool) (Model, error)

// modelPool keeps at most one model running and stops it when nobody has needed it for
// a while (plan section 5): indexing keeps it IndexIdle after its last use, searching
// SearchIdle. It is never kept loaded for good (user decision, 2026-10-13).
type modelPool struct {
	start StartModel
	now   func() time.Time

	mu        sync.Mutex
	model     Model
	starting  chan struct{} // non-nil while a start is in progress; closed when it ends
	startErr  error         // result of the last start
	users     int           // leases not yet released
	keepUntil time.Time     // stop no earlier than this
}

// get returns the running model, starting it if needed. Callers that arrive while a
// start is in progress wait for that one start. keep extends how long the model stays
// loaded after the returned release function is called.
func (p *modelPool) get(ctx context.Context, keep time.Duration, lowPriority bool) (Model, func(), error) {
	for {
		p.mu.Lock()
		if p.model != nil {
			m := p.lease(keep)
			p.mu.Unlock()
			return m, p.releaser(keep), nil
		}
		if p.starting == nil {
			// This caller starts the model; others wait on the channel.
			done := make(chan struct{})
			p.starting = done
			p.mu.Unlock()
			// The start outlives this caller's ctx on purpose: another caller may be
			// waiting for it. It still ends when the model is up or fails.
			m, err := p.start(context.WithoutCancel(ctx), lowPriority)
			p.mu.Lock()
			p.model, p.startErr, p.starting = m, err, nil
			close(done)
			if err != nil {
				p.model = nil
				p.mu.Unlock()
				return nil, nil, err
			}
			p.mu.Unlock()
			continue
		}
		wait := p.starting
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		p.mu.Lock()
		err := p.startErr
		up := p.model != nil
		p.mu.Unlock()
		if !up && err != nil {
			return nil, nil, err
		}
	}
}

// lease records a user; called with mu held.
func (p *modelPool) lease(keep time.Duration) Model {
	p.users++
	p.extend(keep)
	return p.model
}

func (p *modelPool) extend(keep time.Duration) {
	if t := p.now().Add(keep); t.After(p.keepUntil) {
		p.keepUntil = t
	}
}

func (p *modelPool) releaser(keep time.Duration) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.users--
			p.extend(keep) // idle time counts from the last use, not from the start
			p.mu.Unlock()
		})
	}
}

// startAsync starts the model in the background if it isn't running, so it is ready by
// the time it is needed (e.g. while files are still being read).
func (p *modelPool) startAsync(ctx context.Context, keep time.Duration, lowPriority bool) {
	go func() {
		if _, release, err := p.get(ctx, keep, lowPriority); err == nil {
			release()
		}
	}()
}

// running reports whether the model is loaded.
func (p *modelPool) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model != nil
}

// stopIfIdle stops the model when nobody uses it and its keep time has passed. It
// reports whether it stopped it.
func (p *modelPool) stopIfIdle() bool {
	p.mu.Lock()
	m := p.model
	if m == nil || p.users > 0 || p.now().Before(p.keepUntil) {
		p.mu.Unlock()
		return false
	}
	p.model = nil
	p.mu.Unlock()
	m.Close()
	return true
}

// drop stops the model at once (it failed; the next get starts a fresh one).
func (p *modelPool) drop() {
	p.mu.Lock()
	m := p.model
	p.model = nil
	p.mu.Unlock()
	if m != nil {
		m.Close()
	}
}

// close stops the model and waits for a start in progress to finish first.
func (p *modelPool) close() {
	p.mu.Lock()
	wait := p.starting
	p.mu.Unlock()
	if wait != nil {
		<-wait
	}
	p.drop()
}
