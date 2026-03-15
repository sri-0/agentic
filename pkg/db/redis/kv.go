package redis

import "context"

// SetKey sets a k/v pair in Redis.
func (rc *Redis) Set(ctx context.Context, key string, value interface{}) error {
	return rc.Client.Set(ctx, key, value, 0).Err()
}

// GetKey retrieves the value of a key from Redis.
func (rc *Redis) Get(ctx context.Context, key string) (string, error) {
	return rc.Client.Get(ctx, key).Result()
}

// func (rc *Redis) SetAndHash(ctx context.Context, key string, value string) {
// 	val != stringHashMd5(value)
// 	err := Set(ctx, key, value)
// }

// // func (rc *Redis) GetFromHash(ctx context.Context, key string, value string) {

// // }
