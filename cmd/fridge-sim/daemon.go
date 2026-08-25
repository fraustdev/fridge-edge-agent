package main

import (
	"container/heap"
	"context"
	"log"
	"sync"
	"time"
)

// schedItem is one fridge's next scheduled (simulated-time) action: either
// a vend attempt or a restock visit.
type schedItem struct {
	idx  int
	due  time.Time
	kind schedKind
}

type schedKind int

const (
	schedVend schedKind = iota
	schedRestock
)

type schedHeap []schedItem

func (h schedHeap) Len() int           { return len(h) }
func (h schedHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h schedHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *schedHeap) Push(x any)        { *h = append(*h, x.(schedItem)) }
func (h *schedHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// runDaemon drives fridges indefinitely instead of one fixed batch. A
// single goroutine owns the min-heap of "next simulated vend time" per
// fridge -- the only state that needs synchronizing -- and dispatches due
// events to a bounded worker pool for the actual (network-bound) vend
// attempt, so a burst of due events at a high -speed can't fire unbounded
// concurrent POSTs at the fleet server. (SQLite serializes writes; flooding
// it with concurrent requests produces lock-contention errors, not more
// realistic-looking data.)
//
// Restart behavior: each fridge's in-memory slot inventory always starts
// full. The daemon does not reload prior state from the fleet server on
// restart -- a deliberate simplification, not an oversight (see README).
func runDaemon(ctx context.Context, fridges []*fridgeState, dailyTarget int, speed float64, workers int) {
	wallStart := time.Now()
	simStart := wallStart
	simClock := func() time.Time {
		return simStart.Add(time.Duration(float64(time.Since(wallStart)) * speed))
	}

	taskCh := make(chan schedItem, workers)
	doneCh := make(chan schedItem, len(fridges))

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for t := range taskCh {
				fireVend(fridges[t.idx], t.due)
				doneCh <- t
			}
		}()
	}

	var h schedHeap
	for i, fs := range fridges {
		due := simClock().Add(sampleNextInterval(fs.rng, dailyTarget, fs.loc.Vertical, simClock()))
		heap.Push(&h, schedItem{idx: i, due: due, kind: schedVend})
	}

	log.Printf("daemon mode: %d fridges, speed=%.1fx, %d workers -- Ctrl+C to stop", len(fridges), speed, workers)

	// requeue runs after a vend attempt completes: it schedules that
	// fridge's next vend, and separately -- if the fridge has run low
	// enough -- a restock visit some simulated dispatch delay later. A
	// restock is scheduled independently of the regular vend cadence, so
	// it doesn't distort the venue-aware traffic pattern.
	requeue := func(t schedItem) {
		fs := fridges[t.idx]
		next := t.due.Add(sampleNextInterval(fs.rng, dailyTarget, fs.loc.Vertical, t.due))
		heap.Push(&h, schedItem{idx: t.idx, due: next, kind: schedVend})

		if needsRestock(fs) {
			fs.restockPending = true
			delay := time.Duration((1 + fs.rng.Float64()*4) * float64(time.Hour))
			heap.Push(&h, schedItem{idx: t.idx, due: t.due.Add(delay), kind: schedRestock})
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case t := <-doneCh:
			requeue(t)
			continue
		default:
		}

		if h.Len() == 0 {
			select {
			case <-ctx.Done():
				break loop
			case t := <-doneCh:
				requeue(t)
			}
			continue
		}

		top := h[0]
		wait := time.Duration(float64(top.due.Sub(simClock())) / speed)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				break loop
			case t := <-doneCh:
				timer.Stop()
				requeue(t)
				continue
			case <-timer.C:
			}
		}

		item := heap.Pop(&h).(schedItem)
		if item.kind == schedRestock {
			fs := fridges[item.idx]
			restockFridge(fs)
			fs.restockPending = false
			log.Printf("restocked %s (%s)", fs.id, fs.loc.Vertical)
			continue
		}

		select {
		case taskCh <- item:
		case <-ctx.Done():
			break loop
		}
	}

	log.Printf("daemon stopping: waiting for in-flight events to finish...")
	close(taskCh)
	wg.Wait()
	log.Printf("daemon stopped cleanly")
}
