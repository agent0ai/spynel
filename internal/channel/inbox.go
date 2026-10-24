package channel

import (
	"context"
	"sync"
)

// RunInbox keeps ordinary preparation ordered while authored commands reach the
// same handler independently. Callers own bounded queues and reject overflow.
func RunInbox[T any](ctx context.Context, messages, commands <-chan T, handle func(context.Context, T)) {
	var workers sync.WaitGroup
	for _, queue := range []<-chan T{messages, commands} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case message := <-queue:
					if ctx.Err() != nil {
						return
					}
					handle(ctx, message)
				}
			}
		}()
	}
	workers.Wait()
}
