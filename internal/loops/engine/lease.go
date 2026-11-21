package engine

import (
	"context"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

type StorageLease struct {
	Locks *storage.LocksRepository
	Key   string
	Owner string
	TTL   time.Duration
	Now   func() time.Time
}

func (l StorageLease) Acquire(ctx context.Context) (bool, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	ttl := l.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	t := now().UTC()
	reason := "lifecycle_reconcile"
	return l.Locks.Acquire(ctx, storage.LockRecord{Key: l.Key, Owner: l.Owner, Reason: &reason, ExpiresAt: t.Add(ttl).Format("2006-01-02T15:04:05.000Z"), CreatedAt: t.Format("2006-01-02T15:04:05.000Z"), UpdatedAt: t.Format("2006-01-02T15:04:05.000Z")})
}

func (l StorageLease) Release(ctx context.Context) error {
	return l.Locks.ReleaseOwned(ctx, l.Key, l.Owner)
}
