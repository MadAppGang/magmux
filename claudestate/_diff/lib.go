package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/verdict"
)

// fixedListing feeds the library the bytes tt's stub printed, or the failure
// a missing `claude` is.
type fixedListing struct {
	raw []byte
	at  time.Time
	err error
}

func (l fixedListing) List(context.Context) (ports.Listing, error) {
	if l.err != nil {
		return ports.Listing{}, l.err
	}
	return ports.Listing{Raw: l.raw, At: l.at}, nil
}

var errNoClaude = errors.New("claude agents --json: exit status 127 (the sandbox's claude stub)")

// runLibrary is §8.2 step 7: the library over the same root tt read, with
// the real kill(pid, 0) oracle, the same listing, tt's attention rules, the
// document epoch pinned to tt's observedAt, the agents source refreshed
// first, and no watcher.
func runLibrary(root ports.Root, oracle ports.ProcessOracle, listing ports.AgentsListing, now time.Time) (pipeline.Delivery, error) {
	p, err := pipeline.Start(pipeline.Config{
		Root:    root,
		Oracle:  oracle,
		Agents:  listing,
		Verdict: verdict.Rules{IgnoreAPIErrors: true},
		Watch:   false,
		Clock:   ports.RealClock(),
		Now:     func() time.Time { return now },
	})
	if err != nil {
		return pipeline.Delivery{}, err
	}
	defer p.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sub, err := p.Subscribe(ctx, "_diff", 16)
	if err != nil {
		return pipeline.Delivery{}, err
	}
	if listing != nil {
		if _, err := p.RefreshSources(ctx, []model.SourceID{model.SourceAgents}); err != nil {
			return pipeline.Delivery{}, err
		}
		if _, err := next(ctx, sub); err != nil {
			return pipeline.Delivery{}, err
		}
	}
	if !p.Refresh() {
		return pipeline.Delivery{}, pipeline.ErrStopped
	}
	return next(ctx, sub)
}

func next(ctx context.Context, sub *pipeline.Subscription) (pipeline.Delivery, error) {
	select {
	case d, ok := <-sub.C():
		if !ok {
			return pipeline.Delivery{}, pipeline.ErrStopped
		}
		return d, nil
	case <-ctx.Done():
		return pipeline.Delivery{}, fmt.Errorf("the library published nothing: %w", ctx.Err())
	}
}
