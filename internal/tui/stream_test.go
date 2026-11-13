package tui

import (
	"testing"
	"time"

	"jumper/internal/backend"

	"jumper/internal/testload"
)

func TestNextBatchSingleItem(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 7
	items, closed := NextBatch(ch, 50)
	if len(items) != 1 || items[0] != 7 {
		t.Fatalf("items = %v, want [7]", items)
	}
	if closed {
		t.Fatal("closed = true on an open channel")
	}
}

func TestNextBatchClosedWithItems(t *testing.T) {
	ch := make(chan int, 3)
	ch <- 1
	ch <- 2
	ch <- 3
	close(ch)
	items, closed := NextBatch(ch, 10)
	if !closed {
		t.Fatal("closed = false, want true with the remaining items")
	}
	if len(items) != 3 {
		t.Fatalf("items = %v, want 3 items", items)
	}
	for i, want := range []int{1, 2, 3} {
		if items[i] != want {
			t.Fatalf("items[%d] = %d, want %d", i, items[i], want)
		}
	}
}

func TestNextBatchEmptyClosed(t *testing.T) {
	ch := make(chan int)
	close(ch)
	items, closed := NextBatch(ch, 10)
	if !closed {
		t.Fatal("closed = false, want true")
	}
	if len(items) != 0 {
		t.Fatalf("items = %v, want none", items)
	}
}

func TestNextBatchBurst(t *testing.T) {
	const n = 500
	ch := make(chan int, n)
	for i := 0; i < n; i++ {
		ch <- i
	}
	close(ch)
	items, closed := NextBatch(ch, 50)
	if closed {
		t.Fatal("closed = true, want false (more items left)")
	}
	if len(items) == 0 || len(items) > 50 {
		t.Fatalf("len(items) = %d, want 1..50", len(items))
	}
	if items[0] != 0 {
		t.Fatalf("items[0] = %d, want 0", items[0])
	}
}

func TestNextBatchManyItemsCap(t *testing.T) {
	ch := make(chan int, 1000)
	for i := 0; i < 1000; i++ {
		ch <- i
	}
	// Not closed: the drain is capped by max and by the 50 ms window.
	items, closed := NextBatch(ch, 200)
	if closed {
		t.Fatal("closed = true on an open channel")
	}
	if len(items) != 200 {
		t.Fatalf("len(items) = %d, want 200", len(items))
	}
}

func TestNextBatchTrickleBounded(t *testing.T) {
	ch := make(chan int)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case ch <- i:
			case <-stop:
				return
			}
			select {
			case <-time.After(5 * time.Millisecond):
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	start := time.Now()
	items, closed := NextBatch(ch, 100000)
	elapsed := time.Since(start)
	if closed {
		t.Fatal("closed = true, want false")
	}
	if len(items) == 0 {
		t.Fatal("no items returned")
	}
	if elapsed > testload.Scale(150*time.Millisecond) {
		t.Fatalf("NextBatch took %v, want ~50 ms", elapsed)
	}
}

func TestListBatch(t *testing.T) {
	ch := make(chan backend.Result, 250)
	for i := 0; i < 250; i++ {
		ch <- backend.Result{Done: i}
	}
	close(ch)

	msg := ListBatch(3, ch)()
	batch, ok := msg.(ListBatchMsg)
	if !ok {
		t.Fatalf("msg is %T, want ListBatchMsg", msg)
	}
	if batch.Seq != 3 {
		t.Fatalf("Seq = %d, want 3", batch.Seq)
	}
	if len(batch.Results) != 200 {
		t.Fatalf("first batch has %d results, want 200", len(batch.Results))
	}
	if batch.Closed {
		t.Fatal("first batch Closed = true")
	}

	msg = ListBatch(3, ch)()
	batch, ok = msg.(ListBatchMsg)
	if !ok {
		t.Fatalf("msg is %T, want ListBatchMsg", msg)
	}
	if len(batch.Results) != 50 {
		t.Fatalf("second batch has %d results, want 50", len(batch.Results))
	}
	if !batch.Closed {
		t.Fatal("second batch Closed = false, want true")
	}
}

func TestFlashAndNavigationCmds(t *testing.T) {
	msg := Flash(FlashWarn, "host %s slow", "web-1")()
	f, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf("msg is %T, want FlashMsg", msg)
	}
	if f.Level != FlashWarn || f.Text != "host web-1 slow" {
		t.Fatalf("FlashMsg = %+v", f)
	}

	if _, ok := Push(nil)().(PushViewMsg); !ok {
		t.Fatal("Push did not produce a PushViewMsg")
	}
	if _, ok := Pop()().(PopViewMsg); !ok {
		t.Fatal("Pop did not produce a PopViewMsg")
	}
}
