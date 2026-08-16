package event

import "sync"

type Bus[T any] struct {
	mu   sync.RWMutex
	subs []subscriber[T]
}

type subscriber[T any] struct {
	ch       chan T
	lossless bool
}

// Subscribe registers a best-effort subscriber. Events are dropped when the
// subscriber's buffer is full so a slow observer cannot block publishers.
func (b *Bus[T]) Subscribe() <-chan T {
	return b.subscribe(false)
}

// SubscribeLossless registers a backpressured subscriber. Publish waits for
// this subscriber when its buffer is full instead of dropping events.
func (b *Bus[T]) SubscribeLossless() <-chan T {
	return b.subscribe(true)
}

func (b *Bus[T]) subscribe(lossless bool) <-chan T {
	ch := make(chan T, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, subscriber[T]{ch: ch, lossless: lossless})
	return ch
}

func (b *Bus[T]) Publish(v T) {
	b.mu.RLock()
	// Subscribers are append-only and existing entries are immutable, so the
	// captured slice remains safe after releasing the lock without allocating
	// once per event. A concurrent append writes beyond this slice's length.
	subs := b.subs
	b.mu.RUnlock()

	for _, sub := range subs {
		if sub.lossless {
			sub.ch <- v
			continue
		}
		select {
		case sub.ch <- v:
		default:
		}
	}
}
