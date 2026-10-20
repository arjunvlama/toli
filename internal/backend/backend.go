// Package backend is the daemon's logic. The MCP server and the admin API are
// thin frontends that call it; neither touches the store directly.
package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/arjunvlama/toli/internal/backend/peers"
	"github.com/arjunvlama/toli/internal/backend/wake"
	"github.com/arjunvlama/toli/internal/store"
	"github.com/arjunvlama/toli/internal/taskclient"
	"golang.org/x/sync/errgroup"
)

// Options tunes the backend's background work. Zero values mean the defaults.
type Options struct {
	PollEvery  time.Duration // how often active tasks are polled (default 3s)
	CheckEvery time.Duration // how often peers' cards are re-fetched (default 30s)
}

type Backend struct {
	store *store.Store
	peers *peers.Service
	tasks *taskclient.Client
	wake  *wake.Service
}

func New(ctx context.Context, st *store.Store, opts Options) (*Backend, error) {
	ps, err := peers.New(ctx, st, opts.CheckEvery)
	if err != nil {
		return nil, err
	}
	b := &Backend{store: st, peers: ps, wake: wake.New(st)}
	b.tasks = taskclient.New(st.OutgoingTasks(), ps.Client, taskclient.Options{
		PollEvery: opts.PollEvery,
		OnChange:  b.delegatedChanged,
	})
	return b, nil
}

// IncomingTasks is the store a2asrv keeps peers' tasks in. caller identifies
// the peer a request comes from; each sees only the tasks it created.
func (b *Backend) IncomingTasks(caller func(context.Context) (store.Caller, error)) *store.IncomingTasks {
	return b.store.IncomingTasks(caller)
}

// Run runs the backend's background work until ctx ends.
func (b *Backend) Run(ctx context.Context) error {
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := b.peers.Run(gCtx); err != nil {
			return fmt.Errorf("run peer card checker: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		if err := b.tasks.Run(gCtx); err != nil {
			return fmt.Errorf("run task poller: %w", err)
		}
		return nil
	})

	return g.Wait()
}
