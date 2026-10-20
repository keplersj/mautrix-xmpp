package xmppx

import "sync"

// dispatchQueue runs functions one at a time, in order, on its own goroutine.
// It is unbounded so the read loop never blocks on a slow handler; a handler
// waiting for an IQ response would otherwise deadlock the session.
type dispatchQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []func()
	stopped bool
}

func newDispatchQueue() *dispatchQueue {
	q := &dispatchQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *dispatchQueue) push(fn func()) {
	q.mu.Lock()
	if !q.stopped {
		q.items = append(q.items, fn)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

// stop lets queued items finish, then ends run.
func (q *dispatchQueue) stop() {
	q.mu.Lock()
	q.stopped = true
	q.cond.Signal()
	q.mu.Unlock()
}

func (q *dispatchQueue) run() {
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.stopped {
			q.cond.Wait()
		}
		if len(q.items) == 0 {
			q.mu.Unlock()
			return
		}
		fn := q.items[0]
		q.items[0] = nil
		q.items = q.items[1:]
		q.mu.Unlock()
		fn()
	}
}
