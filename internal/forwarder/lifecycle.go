package forwarder

import (
	"context"
	"log"
	"time"
)

type Session interface {
	Run(context.Context) error
	SetStarter(func(context.Context) error)
}

func (e *Engine) Changed() {
	e.interruptWork()
	select {
	case e.Changes <- struct{}{}:
	default:
	}
}

// Serve owns the connection and the polling timer. With no enabled rules it
// waits on change notifications; an explicit login/bind grants a ten-minute lease.
func (e *Engine) Serve(ctx context.Context, session Session, events <-chan Message) error {
	requests := make(chan chan struct{})
	session.SetStarter(func(callCtx context.Context) error {
		reply := make(chan struct{})
		select {
		case requests <- reply:
		case <-callCtx.Done():
			return callCtx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-reply:
			return nil
		case <-callCtx.Done():
			return callCtx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	jobs := make(chan struct{}, 1)
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-workerCtx.Done():
				return
			case m := <-events:
				jobCtx, cancel := e.jobContext(workerCtx)
				if err := e.Event(jobCtx, m); err != nil && workerCtx.Err() == nil {
					log.Print("forwarder event failed")
				}
				cancel()
			case <-jobs:
				jobCtx, cancel := e.jobContext(workerCtx)
				if err := e.Tick(jobCtx); err != nil && workerCtx.Err() == nil {
					log.Print("forwarder tick failed")
				}
				cancel()
			}
		}
	}()
	var stopSession context.CancelFunc
	var sessionDone chan struct{}
	var ticker *time.Ticker
	var ticks <-chan time.Time
	var lease *time.Timer
	var expires <-chan time.Time
	leased := false
	active := false
	stop := func() {
		if stopSession != nil {
			stopSession()
			<-sessionDone
			stopSession = nil
		}
	}
	defer func() {
		stopWorker()
		stop()
		<-workerDone
		if ticker != nil {
			ticker.Stop()
		}
		if lease != nil {
			lease.Stop()
		}
	}()
	reconcile := func() error {
		if err := e.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM rules WHERE enabled=1)").Scan(&active); err != nil {
			return err
		}
		if active && ticker == nil {
			ticker = time.NewTicker(30 * time.Second)
			ticks = ticker.C
			select {
			case jobs <- struct{}{}:
			default:
			}
		}
		if !active {
			e.interruptWork()
		}
		if !active && ticker != nil {
			ticker.Stop()
			ticker = nil
			ticks = nil
		}
		if (active || leased) && stopSession == nil {
			var runCtx context.Context
			runCtx, stopSession = context.WithCancel(ctx)
			sessionDone = make(chan struct{})
			go func() {
				defer close(sessionDone)
				for runCtx.Err() == nil {
					if err := session.Run(runCtx); err != nil && runCtx.Err() == nil {
						log.Print("MTProto connection failed; reconnecting")
					}
					retry := time.NewTimer(5 * time.Second)
					select {
					case <-runCtx.Done():
						retry.Stop()
						return
					case <-retry.C:
					}
				}
			}()
		}
		if !active && !leased {
			stop()
		}
		return nil
	}
	if err := reconcile(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.Changes:
			if err := reconcile(); err != nil {
				return err
			}
		case reply := <-requests:
			leased = true
			if lease != nil {
				lease.Stop()
			}
			lease = time.NewTimer(10 * time.Minute)
			expires = lease.C
			if err := reconcile(); err != nil {
				return err
			}
			close(reply)
		case <-expires:
			leased = false
			expires = nil
			if err := reconcile(); err != nil {
				return err
			}
		case <-ticks:
			select {
			case jobs <- struct{}{}:
			default:
			}
		}
	}
}

func (e *Engine) interruptWork() {
	e.workMu.Lock()
	defer e.workMu.Unlock()
	if e.workCancel != nil {
		e.workCancel()
	}
}
func (e *Engine) jobContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	e.workMu.Lock()
	e.workCancel = cancel
	e.workMu.Unlock()
	return ctx, cancel
}
