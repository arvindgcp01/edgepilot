package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// Notification kinds carried on the bus.
//
// Every one of these is a hint that durable state changed, never the change
// itself. A dropped message costs latency because a reconciliation loop finds
// the same work in Postgres; it never loses anything.
const (
	// KindCommandQueued tells the replica owning a device's stream that a
	// command is waiting for it.
	KindCommandQueued = "command.queued"
	// KindCommandCancelled asks the owning replica to abort a running command.
	KindCommandCancelled = "command.cancelled"
	// KindConfigChanged asks the owning replica to push fresh policy.
	KindConfigChanged = "config.changed"
	// KindDisconnect asks the owning replica to close a device's stream, after
	// the device is revoked or decommissioned.
	KindDisconnect = "device.disconnect"
	// KindCertificateRevoked invalidates the in-process revocation cache on
	// every replica immediately, rather than waiting for the periodic refresh.
	KindCertificateRevoked = "certificate.revoked"
)

// DeviceNotification is addressed to whichever replica owns a device's stream.
type DeviceNotification struct {
	Kind     string    `json:"kind"`
	DeviceID uuid.UUID `json:"device_id"`
	// CommandID is set for command notifications.
	CommandID *uuid.UUID `json:"command_id,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	// From identifies the publishing replica, so a subscriber can ignore its
	// own messages where that matters.
	From string `json:"from,omitempty"`
}

// NotifyDevice publishes a notification for one device.
//
// It is fire-and-forget by design. A failure here is logged and swallowed: the
// caller has already committed the durable change, and failing their request
// because a hint could not be delivered would turn a latency problem into an
// error the user sees.
func (b *Bus) NotifyDevice(ctx context.Context, n DeviceNotification) {
	n.From = b.replicaID
	payload, err := json.Marshal(n)
	if err != nil {
		b.log.ErrorContext(ctx, "encoding device notification failed",
			slog.String("kind", n.Kind), slog.Any("error", err))
		return
	}
	if err := b.client.Publish(ctx, b.deviceChannel(n.DeviceID), payload).Err(); err != nil {
		// Not an error the caller needs to know about: the owning replica's
		// reconciliation loop will pick the work up within a few seconds.
		b.log.WarnContext(ctx, "publishing device notification failed; "+
			"the owning replica will reconcile instead",
			slog.String("kind", n.Kind),
			slog.String("device_id", n.DeviceID.String()),
			slog.Any("error", err))
	}
}

// SubscribeDevice listens for notifications addressed to one device.
//
// The returned cancel function must be called when the stream closes, or the
// subscription leaks a goroutine and a Redis connection per device — at 10,000
// devices that is the difference between working and exhausting file
// descriptors.
func (b *Bus) SubscribeDevice(ctx context.Context, deviceID uuid.UUID, handle func(DeviceNotification)) (cancel func(), err error) {
	sub := b.client.Subscribe(ctx, b.deviceChannel(deviceID))

	// Subscribe is lazy; this forces the round trip so a failure surfaces here
	// rather than as silence.
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, fmt.Errorf("bus: subscribe to device %s: %w", deviceID, err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var n DeviceNotification
				if err := json.Unmarshal([]byte(msg.Payload), &n); err != nil {
					b.log.WarnContext(ctx, "discarding malformed device notification",
						slog.String("device_id", deviceID.String()), slog.Any("error", err))
					continue
				}
				handle(n)
			}
		}
	}()

	return func() {
		_ = sub.Close()
		<-done
	}, nil
}

// BroadcastNotification goes to every replica rather than one.
type BroadcastNotification struct {
	Kind string `json:"kind"`
	// Fingerprint carries the revoked certificate for KindCertificateRevoked.
	Fingerprint []byte `json:"fingerprint,omitempty"`
	From        string `json:"from,omitempty"`
}

// Broadcast publishes to every replica.
//
// Used for revocation, where waiting up to a refresh interval for every replica
// to notice would leave a revoked device connected for that long.
func (b *Bus) Broadcast(ctx context.Context, n BroadcastNotification) {
	n.From = b.replicaID
	payload, err := json.Marshal(n)
	if err != nil {
		b.log.ErrorContext(ctx, "encoding broadcast failed", slog.Any("error", err))
		return
	}
	if err := b.client.Publish(ctx, b.broadcastChannel(), payload).Err(); err != nil {
		b.log.WarnContext(ctx, "publishing broadcast failed; "+
			"replicas will pick this up on their next refresh",
			slog.String("kind", n.Kind), slog.Any("error", err))
	}
}

// SubscribeBroadcast listens for cluster-wide notifications.
func (b *Bus) SubscribeBroadcast(ctx context.Context, handle func(BroadcastNotification)) (cancel func(), err error) {
	sub := b.client.Subscribe(ctx, b.broadcastChannel())
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, fmt.Errorf("bus: subscribe to broadcast: %w", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var n BroadcastNotification
				if err := json.Unmarshal([]byte(msg.Payload), &n); err != nil {
					b.log.WarnContext(ctx, "discarding malformed broadcast", slog.Any("error", err))
					continue
				}
				handle(n)
			}
		}
	}()

	return func() {
		_ = sub.Close()
		<-done
	}, nil
}

func (b *Bus) deviceChannel(deviceID uuid.UUID) string {
	return b.key("agent", deviceID.String())
}

func (b *Bus) broadcastChannel() string { return b.key("broadcast") }
