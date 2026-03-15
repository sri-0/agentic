package valkey

import (
	"context"
	"time"
)

// Set stores a key-value pair with optional TTL.
func (v *Valkey) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	if ttl > 0 {
		return v.Client.Do(ctx, v.Client.B().Set().Key(key).Value(value).Ex(ttl).Build()).Error()
	}
	return v.Client.Do(ctx, v.Client.B().Set().Key(key).Value(value).Build()).Error()
}

// Get retrieves the value of a key.
func (v *Valkey) Get(ctx context.Context, key string) (string, error) {
	return v.Client.Do(ctx, v.Client.B().Get().Key(key).Build()).ToString()
}
