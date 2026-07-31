package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// EnvRedisURL names the Redis instance integration tests use.
const EnvRedisURL = "EP_TEST_REDIS_URL"

// Redis is an isolated Redis handle for one test.
type Redis struct {
	Client *redis.Client
	// Prefix is unique to this test. Every key the code under test writes is
	// namespaced by it.
	Prefix string
}

// NewRedis returns a Redis client namespaced to the calling test.
//
// Redis has no equivalent of "create a scratch database" that is cheap and
// concurrency-safe — SELECT gives only 16 databases and they are shared state,
// so parallel tests would collide. A per-test key prefix gives the same
// isolation without that limit, and the cleanup deletes exactly what the test
// created rather than issuing FLUSHDB, which would destroy a developer's dev
// stack data along with it.
func NewRedis(t *testing.T) *Redis {
	t.Helper()

	url := strings.TrimSpace(os.Getenv(EnvRedisURL))
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set. CI must provide Redis for integration tests.", EnvRedisURL)
		}
		t.Skipf("%s is not set; run `make dev` and export it to run integration tests", EnvRedisURL)
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvRedisURL, err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", EnvRedisURL, err)
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate prefix: %v", err)
	}
	prefix := "eptest:" + sanitize(t.Name()) + ":" + hex.EncodeToString(suffix) + ":"

	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()

		// SCAN rather than KEYS: KEYS blocks the server for the duration, which
		// on a shared dev instance means blocking every other test too.
		var cursor uint64
		for {
			keys, next, err := client.Scan(cleanupCtx, cursor, prefix+"*", 500).Result()
			if err != nil {
				t.Logf("cleanup: scan %s*: %v", prefix, err)
				break
			}
			if len(keys) > 0 {
				if err := client.Del(cleanupCtx, keys...).Err(); err != nil {
					t.Logf("cleanup: delete keys: %v", err)
				}
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		_ = client.Close()
	})

	return &Redis{Client: client, Prefix: prefix}
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, name)
}
