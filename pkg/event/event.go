package event

import "sync"

type Bus[T any] struct {
	mu   sync.RWMutex
	subs []chan T
}

func (b *Bus[T]) Subscribe() <-chan T {
	ch := make(chan T, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, ch)
	return ch
}

func (b *Bus[T]) Publish(v T) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- v:
		default:
		}
	}
}
