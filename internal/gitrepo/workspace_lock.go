package gitrepo

import (
	"context"
	"sync"
)

type projectLocks struct {
	locks sync.Map
}

type projectLock struct {
	semaphore chan struct{}
}

func newProjectLock() *projectLock {
	return &projectLock{semaphore: make(chan struct{}, 1)}
}

func (l *projectLocks) lock(ctx context.Context, projectID string) (func(), error) {
	value, _ := l.locks.LoadOrStore(projectID, newProjectLock())
	lock := value.(*projectLock)
	select {
	case lock.semaphore <- struct{}{}:
		return func() { <-lock.semaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
