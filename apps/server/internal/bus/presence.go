package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// PresenceTTL is how long a heartbeat keeps a device marked live.
//
// Three times the 15-second heartbeat interval, so a single dropped packet does
// not make a healthy Mac vanish from the dashboard. The durable answer in
// Postgres uses a wider threshold still; this is the live one.
const PresenceTTL = 45 * time.Second

// ErrNotPresent means no replica currently holds the device's stream.
var ErrNotPresent = errors.New("device is not connected")

// MarkPresent records that this replica holds a device's stream.
//
// Called on every heartbeat. This is the write that replaces updating
// last_seen_at in Postgres per heartbeat: at 10,000 devices that would be
// 10,000 row updates every 15 seconds against the table the dashboard reads
// most. Here it is a single SET against an in-memory store, and the durable
// column is flushed in batches instead.
func (b *Bus) MarkPresent(ctx context.Context, deviceID uuid.UUID) error {
	if err := b.client.Set(ctx, b.presenceKey(deviceID), b.replicaID, PresenceTTL).Err(); err != nil {
		return fmt.Errorf("bus: mark %s present: %w", deviceID, err)
	}
	return nil
}

// ClearPresence removes a device's presence when its stream closes.
//
// The delete is conditional on this replica still being the recorded owner. A
// device that reconnected elsewhere between our stream closing and this call
// has already had its presence rewritten by the new owner, and deleting it
// blindly would mark a live device offline.
func (b *Bus) ClearPresence(ctx context.Context, deviceID uuid.UUID) error {
	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0`
	if err := b.client.Eval(ctx, script, []string{b.presenceKey(deviceID)}, b.replicaID).Err(); err != nil &&
		!errors.Is(err, redis.Nil) {
		return fmt.Errorf("bus: clear presence for %s: %w", deviceID, err)
	}
	return nil
}

// OwnerOf returns the replica currently holding a device's stream.
func (b *Bus) OwnerOf(ctx context.Context, deviceID uuid.UUID) (string, error) {
	replica, err := b.client.Get(ctx, b.presenceKey(deviceID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotPresent
	}
	if err != nil {
		return "", fmt.Errorf("bus: read presence for %s: %w", deviceID, err)
	}
	return replica, nil
}

// IsPresent reports whether any replica holds the device's stream.
func (b *Bus) IsPresent(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	_, err := b.OwnerOf(ctx, deviceID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotPresent):
		return false, nil
	default:
		return false, err
	}
}

// PresentDevices filters a set of device ids down to those currently connected.
//
// One pipelined round trip rather than one per device: the fleet table asks
// this for every row it renders, and 500 sequential round trips would dominate
// the response time.
func (b *Bus) PresentDevices(ctx context.Context, deviceIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(deviceIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}

	pipe := b.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(deviceIDs))
	for i, id := range deviceIDs {
		cmds[i] = pipe.Get(ctx, b.presenceKey(id))
	}
	// redis.Nil for an absent key surfaces as an error from Exec even though
	// every command "succeeded"; it is expected, not a failure.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("bus: read presence batch: %w", err)
	}

	out := make(map[uuid.UUID]string, len(deviceIDs))
	for i, cmd := range cmds {
		replica, err := cmd.Result()
		if err != nil {
			continue // absent, hence not present
		}
		out[deviceIDs[i]] = replica
	}
	return out, nil
}

func (b *Bus) presenceKey(deviceID uuid.UUID) string {
	return b.key("presence", "device", deviceID.String())
}
