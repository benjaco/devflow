package event

import (
	"sync"
	"testing"
	"time"
)

func TestBusPublishesToEverySubscriber(t *testing.T) {
	var bus Bus[int]
	first := bus.Subscribe()
	second := bus.Subscribe()
	bus.Publish(42)
	for name, ch := range map[string]<-chan int{"first": first, "second": second} {
		select {
		case got := <-ch:
			if got != 42 {
				t.Fatalf("%s subscriber got %d, want 42", name, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s subscriber did not receive event", name)
		}
	}
}

func TestBusDoesNotBlockOnSlowSubscriber(t *testing.T) {
	var bus Bus[int]
	_ = bus.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1_000; i++ {
			bus.Publish(i)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on a full subscriber buffer")
	}
}

func TestBusConcurrentSubscribeAndPublish(t *testing.T) {
	var bus Bus[int]
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = bus.Subscribe()
		}()
		go func(value int) {
			defer wg.Done()
			bus.Publish(value)
		}(i)
	}
	wg.Wait()
}
