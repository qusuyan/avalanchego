package blockexecutor

import "context"

type workerIDContextKey struct{}

func WithWorkerID(ctx context.Context, workerID int) context.Context {
	return context.WithValue(ctx, workerIDContextKey{}, workerID)
}

func WorkerIDFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	id, ok := ctx.Value(workerIDContextKey{}).(int)
	return id, ok
}

