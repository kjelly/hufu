package main

import "testing"

func TestPromptInjectorEnqueueReportsOverflow(t *testing.T) {
	injector := newPromptInjector(nil)
	capacity := cap(injector.ch)
	for i := range capacity {
		if !injector.enqueue("prompt") {
			t.Fatalf("enqueue %d rejected before the queue was full", i)
		}
	}
	if injector.enqueue("overflow") {
		t.Fatal("enqueue accepted a prompt beyond the queue capacity")
	}
	if got := len(injector.ch); got != capacity {
		t.Fatalf("queue length = %d, want %d", got, capacity)
	}
}
