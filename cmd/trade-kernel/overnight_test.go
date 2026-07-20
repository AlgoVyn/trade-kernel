package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/bars"
	"trade-kernel/internal/session"
)

// testFeed builds an overnightFeed with a scripted trade fetcher (no quote
// fetcher) and returns the feed plus the aggregator it writes into.
func testFeed(t *testing.T, fetch tradeFetcher) (*overnightFeed, *bars.Aggregator) {
	t.Helper()
	return testFeedQ(t, fetch, nil)
}

// testFeedQ is testFeed with an optional quote fetcher.
func testFeedQ(t *testing.T, fetch tradeFetcher, fetchQuote quoteFetcher) (*overnightFeed, *bars.Aggregator) {
	t.Helper()
	agg := bars.NewAggregator(9, 21)
	cursor := &atomic.Value{}
	sym := "TEST"
	feed := newOvernightFeed(agg, session.NewEngine(nil), func() string { return sym }, fetch, fetchQuote, cursor)
	return feed, agg
}

func TestOvernightFeedAppliesNewTradesOnce(t *testing.T) {
	now := time.Now()
	trades := []alpaca.Trade{
		{Symbol: "TEST", ID: 1, Price: 100, Size: 10, Timestamp: now.Add(-3 * time.Second)},
		{Symbol: "TEST", ID: 2, Price: 101, Size: 20, Timestamp: now.Add(-2 * time.Second)},
		{Symbol: "TEST", ID: 3, Price: 102, Size: 30, Timestamp: now.Add(-1 * time.Second)},
	}
	var feed *overnightFeed
	var agg *bars.Aggregator
	feed, agg = testFeed(t, func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
		return trades, nil
	})

	feed.pollOnce(context.Background(), "TEST")
	price, _ := agg.LatestTrade()
	if price != 102 {
		t.Fatalf("last price = %v, want 102", price)
	}

	// Second poll with the same batch: dedupe must block re-application.
	feed.pollOnce(context.Background(), "TEST")
	snap := agg.Snapshot(bars.TF1m, 10, 0)
	if len(snap.Bars) == 0 {
		t.Fatal("no bars after polling")
	}
	last := snap.Bars[len(snap.Bars)-1]
	if last.Volume != 60 {
		t.Fatalf("volume = %v, want 60 (trades applied exactly once)", last.Volume)
	}
}

func TestOvernightFeedRespectsBackfillCursor(t *testing.T) {
	now := time.Now()
	old := alpaca.Trade{Symbol: "TEST", ID: 1, Price: 100, Size: 10, Timestamp: now.Add(-5 * time.Second)}
	new := alpaca.Trade{Symbol: "TEST", ID: 2, Price: 101, Size: 20, Timestamp: now.Add(-1 * time.Second)}
	feed, agg := testFeed(t, func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
		return []alpaca.Trade{old, new}, nil
	})
	// Backfill covered everything up to now-2s: the old print must be skipped.
	feed.backfillCursor.Store(now.Add(-2 * time.Second))

	feed.pollOnce(context.Background(), "TEST")
	snap := agg.Snapshot(bars.TF1m, 10, 0)
	if len(snap.Bars) == 0 {
		t.Fatal("no bars after polling")
	}
	last := snap.Bars[len(snap.Bars)-1]
	if last.Volume != 20 {
		t.Fatalf("volume = %v, want 20 (only the post-cursor trade applied)", last.Volume)
	}
	if last.Close != 101 {
		t.Fatalf("close = %v, want 101", last.Close)
	}
}

func TestOvernightFeedSymbolSwitchResetsState(t *testing.T) {
	now := time.Now()
	feed, agg := testFeed(t, func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
		return []alpaca.Trade{{Symbol: "NEW", ID: 1, Price: 50, Size: 5, Timestamp: now}}, nil
	})
	// Seed state under the old symbol.
	feed.mu.Lock()
	feed.curSym = "OLD"
	feed.cursor = now.Add(-time.Minute)
	feed.seen[999] = struct{}{}
	feed.mu.Unlock()

	feed.pollOnce(context.Background(), "NEW")
	price, _ := agg.LatestTrade()
	if price != 50 {
		t.Fatalf("last price = %v, want 50 after symbol switch", price)
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if feed.curSym != "NEW" {
		t.Fatalf("curSym = %q, want NEW", feed.curSym)
	}
	if _, stale := feed.seen[999]; stale {
		t.Fatal("seen-set kept old-symbol IDs across symbol switch")
	}
}

func TestOvernightFeedDropsBatchAfterMidFetchSwitch(t *testing.T) {
	now := time.Now()
	var feed *overnightFeed
	var agg *bars.Aggregator
	feed, agg = testFeed(t, func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
		// Simulate the symbol changing while the REST call is in flight.
		feed.mu.Lock()
		feed.curSym = "OTHER"
		feed.mu.Unlock()
		return []alpaca.Trade{{Symbol: "TEST", ID: 1, Price: 100, Size: 10, Timestamp: now}}, nil
	})
	feed.pollOnce(context.Background(), "TEST")
	if price, _ := agg.LatestTrade(); price != 0 {
		t.Fatalf("last price = %v, want 0 (batch from superseded symbol dropped)", price)
	}
}

func TestOvernightFeedAdvancesCursorForOutOfOrderFetch(t *testing.T) {
	now := time.Now()
	// Fetcher returns trades unsorted-ish within a batch (ascending per API,
	// but verify cursor ends at the max).
	feed, _ := testFeed(t, func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
		return []alpaca.Trade{
			{Symbol: "TEST", ID: 1, Price: 100, Size: 10, Timestamp: now.Add(-2 * time.Second)},
			{Symbol: "TEST", ID: 2, Price: 101, Size: 10, Timestamp: now.Add(-1 * time.Second)},
		}, nil
	})
	feed.pollOnce(context.Background(), "TEST")
	feed.mu.Lock()
	cursor := feed.cursor
	feed.mu.Unlock()
	if !cursor.Equal(now.Add(-1 * time.Second)) {
		t.Fatalf("cursor = %v, want %v", cursor, now.Add(-1*time.Second))
	}
}

func TestOvernightFeedAppliesQuote(t *testing.T) {
	now := time.Now()
	feed, agg := testFeedQ(t,
		func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
			return nil, nil
		},
		func(_ context.Context, _ string) (alpaca.Quote, error) {
			return alpaca.Quote{Symbol: "TEST", BidPrice: 99.5, AskPrice: 100.5, Timestamp: now}, nil
		})
	feed.pollOnce(context.Background(), "TEST")
	bid, ask, at := agg.LatestQuote()
	if bid != 99.5 || ask != 100.5 || !at.Equal(now) {
		t.Fatalf("quote = %vx%v at %v, want 99.5x100.5 at %v", bid, ask, at, now)
	}
}

func TestOvernightFeedSkipsZeroQuote(t *testing.T) {
	feed, agg := testFeedQ(t,
		func(_ context.Context, _ string, _, _ time.Time, _ int) ([]alpaca.Trade, error) {
			return nil, nil
		},
		func(_ context.Context, _ string) (alpaca.Quote, error) {
			return alpaca.Quote{Symbol: "TEST"}, nil // no bid/ask
		})
	feed.pollOnce(context.Background(), "TEST")
	bid, ask, _ := agg.LatestQuote()
	if bid != 0 || ask != 0 {
		t.Fatalf("quote = %vx%v, want 0x0 (zero quote not cached)", bid, ask)
	}
}
