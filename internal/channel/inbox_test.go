package channel

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestInboxCommandsBypassBlockedHandlerAndShutdownDropsPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		messages, commands := make(chan int, 64), make(chan int, 64)
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		handled := make(chan int, 8)
		go func() {
			RunInbox(ctx, messages, commands, func(_ context.Context, n int) {
				if n == 1 {
					close(entered)
					<-release
				}
				handled <- n
			})
			close(done)
		}()
		messages <- 1
		<-entered
		messages <- 2
		messages <- 3
		commands <- 4
		if got := <-handled; got != 4 {
			t.Fatalf("command blocked behind handler: %d", got)
		}
		cancel()
		close(release)
		<-done
		if got := <-handled; got != 1 {
			t.Fatalf("active handler: %d", got)
		}
		select {
		case got := <-handled:
			t.Fatalf("pending work ran after shutdown: %d", got)
		default:
		}
	})
}
