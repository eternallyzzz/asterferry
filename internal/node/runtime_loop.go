package node

import (
	controlwire "asterferry/internal/controlwire"
	v1 "asterferry/internal/controlwire/v1"
	"asterferry/internal/domain"
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const (
	controllerReconnectInitialBackoff = time.Second
	controllerReconnectMaxBackoff     = 5 * time.Second
)

// Run maintains a controller stream with bounded reconnect backoff. A
// disconnected node keeps the last encrypted snapshot active and reports a
// degraded observed state only after the grace interval.
func (r *Runtime) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.runtimeMu.Lock()
	r.runCtx = ctx
	dataPlane := r.dataPlane
	r.runtimeMu.Unlock()
	defer func() {
		_ = r.disableRuntime()
	}()
	if dataPlane != nil {
		if err := dataPlane.Start(ctx); err != nil {
			return err
		}
	}
	backoff := controllerReconnectInitialBackoff
	const offlineGrace = 30 * time.Second
	var disconnectedSince time.Time
	for {
		err := r.runConnection(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			// A normal transport outage deliberately leaves the last data
			// generation serving traffic. PermissionDenied, on the other hand,
			// is the Controller's authoritative signal that this identity was
			// revoked/disabled or its certificate serial is no longer current;
			// invalidate sessions and keep the engine drained until a future
			// authenticated stream succeeds.
			if status.Code(err) == codes.PermissionDenied {
				if engine := r.Engine(); engine != nil {
					engine.BeginDrain()
				}
				if dataPlane := r.DataPlane(); dataPlane != nil {
					dataPlane.CloseSessions()
				}
			}
			r.logger.Warn("control connection lost", "error", err)
			now := time.Now().UTC()
			if disconnectedSince.IsZero() {
				disconnectedSince = now
			}
			r.reconciler.MarkDisconnected(now, offlineGrace)
			wait := jitteredControllerReconnectDelay(backoff)
			if remaining := offlineGrace - now.Sub(disconnectedSince); remaining > 0 && remaining < wait {
				wait = remaining
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return nil
			}
			if time.Since(disconnectedSince) >= offlineGrace {
				r.reconciler.MarkDisconnected(time.Now().UTC(), offlineGrace)
			}
			if backoff < controllerReconnectMaxBackoff {
				backoff *= 2
				if backoff > controllerReconnectMaxBackoff {
					backoff = controllerReconnectMaxBackoff
				}
			}
			continue
		}
		disconnectedSince = time.Time{}
		// A connection normally ends only with an error. If an implementation
		// returns nil, still apply a bounded delay so a graceful server close
		// cannot turn into a hot reconnect loop.
		if backoff > controllerReconnectInitialBackoff {
			backoff = controllerReconnectInitialBackoff
		}
		if !waitWithContext(ctx, backoff) {
			return nil
		}
	}
}

// jitteredControllerReconnectDelay keeps a fleet of Nodes from reconnecting
// in one burst when the active Controller changes. The jitter is deliberately
// bounded to +/-20% and is applied only to the control-plane retry loop.
func jitteredControllerReconnectDelay(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	factor := 0.8 + 0.4*rand.Float64()
	delay := time.Duration(float64(base) * factor)
	if delay <= 0 {
		return time.Nanosecond
	}
	return delay
}

func waitWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runtime) runConnection(ctx context.Context) error {
	// A blocking gRPC dial must have its own bounded attempt deadline. Using
	// the process context directly would leave a node stuck forever in
	// DialContext while the Controller is down, preventing the offline cache
	// grace/degraded state and reconnect backoff from taking effect.
	bootstrap := r.bootstrapSnapshot()
	dialCtx, cancelDial := context.WithTimeout(ctx, controllerDialTimeout)
	client, conn, err := ControlClient(dialCtx, bootstrap)
	cancelDial()
	if err != nil {
		return err
	}
	defer conn.Close()
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	stream, err := client.Connect(connectionCtx)
	if err != nil {
		return err
	}
	if err := r.reconciler.SetNodeID(bootstrap.NodeID); err != nil {
		return err
	}
	var sendMu sync.Mutex
	send := func(message *v1.NodeMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(message)
	}
	state := r.observedState()
	appliedChecksum := r.reconciler.AppliedChecksum()
	if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_Hello{Hello: &v1.Hello{NodeId: bootstrap.NodeID, SchemaVersion: domain.CurrentControlProtocolVersion, AppliedGeneration: state.AppliedGeneration, AppliedChecksum: appliedChecksum, Capabilities: []string{"tcp", "udp", "http", "socks5", "runtime-telemetry-v1", "runtime-control-v1"}}}}); err != nil {
		return err
	}
	r.reconciler.MarkConnected(time.Now().UTC())
	if renewal, renewalErr := renewalRequest(bootstrap); renewalErr == nil && renewal != nil {
		if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_RenewCertificate{RenewCertificate: &v1.RenewCertificate{CsrDer: renewal}}}); err != nil {
			return err
		}
	}
	// Report the cached/initial state immediately; the Controller should not
	// have to wait for the first desired snapshot to know that this node is
	// connected.
	if observed, observedErr := controlwire.ObservedToProto(state); observedErr == nil {
		if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ObservedState{ObservedState: observed}}); err != nil {
			return err
		}
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(connectionCtx)
	defer cancelHeartbeat()
	var runtimeTelemetryStarted atomic.Bool
	startRuntimeTelemetry := func() {
		if runtimeTelemetryStarted.Swap(true) {
			return
		}
		go r.runtimeTelemetryLoop(connectionCtx, send)
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				observed := r.observedState()
				if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_Heartbeat{Heartbeat: controlwire.Heartbeat(observed.AppliedGeneration, observed.Healthy)}}); err != nil {
					r.logger.Warn("node heartbeat send failed", "error", err)
					// Recv is blocked in the main goroutine. Cancelling only the
					// heartbeat child would leave the control RPC alive forever;
					// cancel the stream context so Recv observes the failure too.
					cancelConnection()
					return
				}
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if message == nil {
			return errors.New("controller sent an empty control message")
		}
		if action := message.GetAction(); action != nil {
			switch action.GetName() {
			case "session_ready":
				// The Controller sent this only after authenticating Hello and
				// checking the current certificate serial.  Do not clear a
				// reconnect/revocation drain before that acknowledgement.
				if engine := r.Engine(); engine != nil {
					engine.EndDrain()
				}
				if runtimeActionAllows(action, "runtime-telemetry-v1") {
					startRuntimeTelemetry()
				}
			case "runtime_connection", "clear_runtime_controls":
				dataPlane := r.DataPlane()
				var affected int
				var actionErr error
				if dataPlane == nil {
					actionErr = errors.New("data-plane runtime is not configured")
				} else if !runtimeTelemetryStarted.Load() && action.GetName() == "runtime_connection" {
					actionErr = errors.New("runtime control capability was not negotiated")
				} else {
					affected, actionErr = dataPlane.ApplyRuntimeAction(ctx, action.GetName(), action.GetPayloadJson())
				}
				if err := r.sendRuntimeActionResult(send, action.GetId(), action.GetName(), affected, actionErr); err != nil {
					return err
				}
			case "drain":
				if engine := r.Engine(); engine != nil {
					engine.BeginDrain()
				}
			case "resync":
				// Resync is a deliberate same-generation repair. Ordinary desired
				// snapshot delivery remains strictly monotonic, while Resync lets
				// the data-plane rebuild indexes after a local component restart. Keep
				// admission closed until the complete resync succeeds; reopening before
				// the component swap would expose a half-rebuilt generation.
				result := r.reconciler.Resync(ctx)
				if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_APPLIED {
					if engine := r.Engine(); engine != nil {
						engine.EndDrain()
					}
				}
				if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: result}}); err != nil {
					return err
				}
			case "reconnect":
				// Keep the engine drained until a new authenticated Controller
				// stream is established. This is important for certificate
				// revocation: a node must not reconnect to a peer using its old
				// still-CA-valid certificate while it is offline from the
				// authority that revoked it.
				if engine := r.Engine(); engine != nil {
					engine.BeginDrain()
				}
				// An explicit reconnect is also the Controller's revocation
				// signal.  Invalidate authenticated AFDP sessions immediately,
				// while retaining listeners and the cached generation so a
				// temporary control outage still preserves data-plane traffic.
				if dataPlane := r.DataPlane(); dataPlane != nil {
					dataPlane.CloseSessions()
				}
				return errors.New("controller requested reconnect")
			default:
				// Unknown actions are operational hints. Ignore them so a newer
				// Controller can add actions without taking an older node offline.
			}
		}
		if desired := message.GetDesiredSnapshot(); desired != nil {
			snapshot, decodeErr := controlwire.SnapshotFromProto(desired)
			if decodeErr != nil {
				rejected := &v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: controlwire.ApplyResult(desired.Generation, desired.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyError(decodeErr))}}
				if sendErr := send(rejected); sendErr != nil {
					r.logger.Warn("rejected snapshot result send failed", "generation", desired.Generation, "error", sendErr)
					return errors.Join(decodeErr, sendErr)
				}
				return decodeErr
			}
			// ACCEPTED means the complete envelope has passed schema, metadata
			// and checksum validation; malformed snapshots are reported only as
			// REJECTED and never acknowledged as accepted work.
			if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: controlwire.ApplyResult(desired.Generation, desired.Checksum, v1.ApplyStatus_APPLY_STATUS_ACCEPTED, nil)}}); err != nil {
				return err
			}
			result := r.reconciler.Apply(ctx, snapshot)
			if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: result}}); err != nil {
				return err
			}
			observed, observedErr := controlwire.ObservedToProto(r.observedState())
			if observedErr != nil {
				return fmt.Errorf("encode observed state: %w", observedErr)
			}
			if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ObservedState{ObservedState: observed}}); err != nil {
				return err
			}
		}
		if certificate := message.GetCertificateBundle(); certificate != nil {
			if err := r.acceptCertificate(certificate); err != nil {
				return err
			}
		}
		if message.GetAction() == nil && message.GetDesiredSnapshot() == nil && message.GetCertificateBundle() == nil {
			return errors.New("controller sent an empty control message")
		}
	}
}
