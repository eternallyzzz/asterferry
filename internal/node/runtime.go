package node

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"asterferry/internal/control"
	v1 "asterferry/internal/control/v1"
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type RuntimeOptions struct {
	Logger        *slog.Logger
	CachePath     string
	CacheKeyPath  string
	BootstrapPath string
	MaxStreams    int
	MaxSessions   int
}

type Runtime struct {
	bootstrap     Bootstrap
	bootstrapMu   sync.RWMutex
	engine        *dataplane.Engine
	dataPlane     *DataPlaneRuntime
	reconciler    *Reconciler
	logger        *slog.Logger
	bootstrapPath string
}

func NewRuntime(bootstrap Bootstrap, options RuntimeOptions) (*Runtime, error) {
	if err := validateBootstrap(bootstrap); err != nil {
		return nil, err
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	cachePath := options.CachePath
	if cachePath == "" {
		cachePath = bootstrap.CachePath
	}
	if cachePath == "" {
		cachePath = filepath.Join("state", bootstrap.NodeID, "snapshot.cache")
	}
	keyPath := options.CacheKeyPath
	if keyPath == "" {
		keyPath = filepath.Join(filepath.Dir(cachePath), "snapshot.key")
	}
	key, err := loadOrCreateNodeKey(keyPath)
	if err != nil {
		return nil, err
	}
	cache, err := NewSnapshotCache(cachePath, key)
	if err != nil {
		return nil, err
	}
	engine, err := dataplane.New(dataplane.Options{Role: bootstrap.Role, NodeID: bootstrap.NodeID, MaxStreams: options.MaxStreams, MaxSessions: options.MaxSessions})
	if err != nil {
		return nil, err
	}
	dataPlane, err := NewDataPlaneRuntime(DataPlaneOptions{Engine: engine, Bootstrap: bootstrap, Logger: options.Logger})
	if err != nil {
		return nil, err
	}
	apply := func(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
		// Hold admission closed across the complete component swap. The engine
		// index and the listener/session set are published by separate
		// components; without this gate a new open could observe one half of a
		// generation while the other half is still being rebuilt.
		wasDraining := engine.IsDraining()
		engine.BeginDrain()
		defer func() {
			if !wasDraining {
				engine.EndDrain()
			}
		}()
		if err := engine.ApplySnapshot(ctx, snapshot, previous); err != nil {
			return err
		}
		if err := dataPlane.ApplySnapshot(ctx, snapshot, previous); err != nil {
			// Keep the control engine and network adapters on the same
			// generation if opening a listener/session fails.
			if previous != nil {
				_ = engine.ApplySnapshot(ctx, *previous, &snapshot)
				_ = dataPlane.ApplySnapshot(context.Background(), *previous, nil)
			} else {
				// The first engine apply has no previous document to restore. Clear
				// the speculative index so a rejected listener build cannot leave
				// authorization state active without a matching data-plane socket.
				_ = engine.ResetSnapshot(snapshot.Generation)
			}
			return err
		}
		return nil
	}
	reset := func(ctx context.Context, generation uint64) error {
		// Clear network ownership and authorization together.  The reconciler
		// invokes this only when there is no durable previous snapshot to
		// restore (for example, the first cache publication failed).
		dataErr := dataPlane.ResetSnapshot(generation)
		engineErr := engine.ResetSnapshot(generation)
		return errors.Join(dataErr, engineErr)
	}
	reconciler, err := NewReconcilerWithReset(cache, apply, reset)
	if err != nil {
		return nil, err
	}
	if snapshot, readErr := cache.Read(); readErr == nil {
		if applyErr := engine.ApplySnapshot(context.Background(), snapshot, nil); applyErr != nil {
			return nil, fmt.Errorf("apply cached snapshot: %w", applyErr)
		}
		if applyErr := dataPlane.ApplySnapshot(context.Background(), snapshot, nil); applyErr != nil {
			return nil, fmt.Errorf("prepare cached data-plane snapshot: %w", applyErr)
		}
	}
	if err := reconciler.SetNodeID(bootstrap.NodeID); err != nil {
		return nil, err
	}
	return &Runtime{bootstrap: bootstrap, engine: engine, dataPlane: dataPlane, reconciler: reconciler, logger: options.Logger, bootstrapPath: options.BootstrapPath}, nil
}

func (r *Runtime) Engine() *dataplane.Engine    { return r.engine }
func (r *Runtime) DataPlane() *DataPlaneRuntime { return r.dataPlane }
func (r *Runtime) Reconciler() *Reconciler      { return r.reconciler }

func (r *Runtime) observedState() domain.ObservedState {
	observed := r.reconciler.State()
	if r.dataPlane == nil {
		return observed
	}
	dataObserved, ok := r.dataPlane.ObservedState()
	if !ok {
		return observed
	}
	observed.Sessions = dataObserved.Sessions
	observed.Listeners = dataObserved.Listeners
	observed.Metrics = dataObserved.Metrics
	if observed.AppliedGeneration == dataObserved.AppliedGeneration && !dataObserved.Healthy {
		observed.Healthy = false
		observed.Degraded = true
	}
	observed.ObservedAt = dataObserved.ObservedAt
	return observed
}

// Run maintains a controller stream with bounded reconnect backoff. A
// disconnected node keeps the last encrypted snapshot active and reports a
// degraded observed state only after the grace interval.
func (r *Runtime) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.dataPlane != nil {
		if err := r.dataPlane.Start(ctx); err != nil {
			return err
		}
		defer r.dataPlane.Close()
	}
	backoff := time.Second
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
				r.engine.BeginDrain()
				if r.dataPlane != nil {
					r.dataPlane.CloseSessions()
				}
			}
			r.logger.Warn("control connection lost", "error", err)
			now := time.Now().UTC()
			if disconnectedSince.IsZero() {
				disconnectedSince = now
			}
			r.reconciler.MarkDisconnected(now, offlineGrace)
			wait := backoff
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
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
			continue
		}
		disconnectedSince = time.Time{}
		// A connection normally ends only with an error. If an implementation
		// returns nil, still apply a bounded delay so a graceful server close
		// cannot turn into a hot reconnect loop.
		if backoff > time.Second {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *Runtime) runConnection(ctx context.Context) error {
	// A blocking gRPC dial must have its own bounded attempt deadline. Using
	// the process context directly would leave a node stuck forever in
	// DialContext while the Controller is down, preventing the offline cache
	// grace/degraded state and reconnect backoff from taking effect.
	bootstrap := r.bootstrapSnapshot()
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	client, conn, err := ControlClient(dialCtx, bootstrap)
	cancelDial()
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := client.Connect(ctx)
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
	role := v1.NodeRole_NODE_ROLE_GATEWAY
	if bootstrap.Role == domain.RoleAgent {
		role = v1.NodeRole_NODE_ROLE_AGENT
	}
	if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_Hello{Hello: &v1.Hello{NodeId: bootstrap.NodeID, Role: role, SchemaVersion: domain.SchemaVersion, AppliedGeneration: state.AppliedGeneration, AppliedChecksum: appliedChecksum, Capabilities: []string{"tcp", "udp", "http", "socks5"}}}}); err != nil {
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
	if observed, observedErr := control.ObservedToProto(state); observedErr == nil {
		if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ObservedState{ObservedState: observed}}); err != nil {
			return err
		}
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				observed := r.observedState()
				_ = send(&v1.NodeMessage{Body: &v1.NodeMessage_Heartbeat{Heartbeat: control.Heartbeat(bootstrap.NodeID, observed.AppliedGeneration, observed.Healthy)}})
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
			return errors.New("Controller sent an empty control message")
		}
		if action := message.GetAction(); action != nil {
			switch action.GetName() {
			case "session_ready":
				// The Controller sent this only after authenticating Hello and
				// checking the current certificate serial.  Do not clear a
				// reconnect/revocation drain before that acknowledgement.
				r.engine.EndDrain()
			case "drain":
				r.engine.BeginDrain()
			case "resync":
				// Resync is a deliberate same-generation repair. Ordinary desired
				// snapshot delivery remains strictly monotonic, while Resync lets
				// the data-plane rebuild indexes after a local component restart. Keep
				// admission closed until the complete resync succeeds; reopening before
				// the component swap would expose a half-rebuilt generation.
				result := r.reconciler.Resync(ctx)
				if result.GetStatus() == v1.ApplyStatus_APPLY_STATUS_APPLIED {
					r.engine.EndDrain()
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
				r.engine.BeginDrain()
				// An explicit reconnect is also the Controller's revocation
				// signal.  Invalidate authenticated AFDP sessions immediately,
				// while retaining listeners and the cached generation so a
				// temporary control outage still preserves data-plane traffic.
				if r.dataPlane != nil {
					r.dataPlane.CloseSessions()
				}
				return errors.New("Controller requested reconnect")
			default:
				// Unknown actions are operational hints. Ignore them so a newer
				// Controller can add actions without taking an older node offline.
			}
		}
		if desired := message.GetDesiredSnapshot(); desired != nil {
			snapshot, decodeErr := control.SnapshotFromProto(desired)
			if decodeErr != nil {
				_ = send(&v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: control.ApplyResult(desired.Generation, desired.Checksum, v1.ApplyStatus_APPLY_STATUS_REJECTED, applyError(decodeErr))}})
				return decodeErr
			}
			// ACCEPTED means the complete envelope has passed schema, metadata
			// and checksum validation; malformed snapshots are reported only as
			// REJECTED and never acknowledged as accepted work.
			if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: control.ApplyResult(desired.Generation, desired.Checksum, v1.ApplyStatus_APPLY_STATUS_ACCEPTED, nil)}}); err != nil {
				return err
			}
			result := r.reconciler.Apply(ctx, snapshot)
			if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ApplyResult{ApplyResult: result}}); err != nil {
				return err
			}
			observed, _ := control.ObservedToProto(r.observedState())
			if observed != nil {
				if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ObservedState{ObservedState: observed}}); err != nil {
					return err
				}
			}
		}
		if certificate := message.GetCertificateBundle(); certificate != nil {
			if err := r.acceptCertificate(certificate); err != nil {
				return err
			}
		}
		if message.GetAction() == nil && message.GetDesiredSnapshot() == nil && message.GetCertificateBundle() == nil {
			return errors.New("Controller sent an empty control message")
		}
	}
}

func renewalRequest(bootstrap Bootstrap) ([]byte, error) {
	certificate, err := tls.X509KeyPair([]byte(bootstrap.CertificatePEM), []byte(bootstrap.PrivateKeyPEM))
	if err != nil || len(certificate.Certificate) == 0 {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	if leaf.NotAfter.After(time.Now().UTC().Add(7 * 24 * time.Hour)) {
		return nil, nil
	}
	return GenerateCSRWithPrivateKey(bootstrap.NodeID, bootstrap.Role, []byte(bootstrap.PrivateKeyPEM))
}

func (r *Runtime) acceptCertificate(bundle *v1.CertificateBundle) error {
	if bundle == nil || len(bundle.CertificateDer) == 0 {
		return errors.New("Controller returned an empty certificate bundle")
	}
	leaf, err := x509.ParseCertificate(bundle.CertificateDer)
	if err != nil {
		return fmt.Errorf("Controller returned an invalid certificate: %w", err)
	}
	if strings.TrimSpace(bundle.Serial) == "" || !strings.EqualFold(leaf.SerialNumber.Text(16), strings.TrimSpace(bundle.Serial)) {
		return errors.New("Controller certificate serial does not match the bundle")
	}
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("Controller certificate is expired or not yet valid")
	}
	bootstrap := r.bootstrapSnapshot()
	if leaf.Subject.CommonName != bootstrap.NodeID {
		return errors.New("Controller certificate identity does not match the node")
	}
	keyPair, err := tls.X509KeyPair([]byte(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CertificateDer})), []byte(bootstrap.PrivateKeyPEM))
	if err != nil || len(keyPair.Certificate) == 0 {
		return errors.New("Controller certificate does not match the node private key")
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CertificateDer})
	caPEM := []byte(bootstrap.CAPEM)
	if len(bundle.CaCertificateDer) > 0 {
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CaCertificateDer})
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("Controller certificate bundle has no valid CA")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: now}); err != nil {
		return fmt.Errorf("Controller certificate is not signed by the configured CA: %w", err)
	}
	updated := bootstrap
	updated.CertificatePEM = string(certificatePEM)
	if len(caPEM) > 0 {
		updated.CAPEM = string(caPEM)
	}
	if r.bootstrapPath != "" {
		if err := WriteBootstrap(r.bootstrapPath, updated); err != nil {
			return err
		}
	}
	if r.dataPlane != nil {
		if err := r.dataPlane.UpdateBootstrap(updated); err != nil {
			if r.bootstrapPath != "" {
				_ = WriteBootstrap(r.bootstrapPath, bootstrap)
			}
			_ = r.dataPlane.UpdateBootstrap(bootstrap)
			return err
		}
	}
	r.bootstrapMu.Lock()
	r.bootstrap = updated
	r.bootstrapMu.Unlock()
	return nil
}

func (r *Runtime) bootstrapSnapshot() Bootstrap {
	r.bootstrapMu.RLock()
	defer r.bootstrapMu.RUnlock()
	return r.bootstrap
}

func applyError(err error) *domain.ApplyError {
	var value *domain.ApplyError
	if errors.As(err, &value) {
		return value
	}
	return &domain.ApplyError{Code: "invalid_snapshot", Message: err.Error()}
}

func loadOrCreateNodeKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != 32 {
			return nil, errors.New("node cache key must contain 32 bytes")
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".node-key-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(name, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && len(existing) == 32 {
			return existing, nil
		}
		return nil, fmt.Errorf("publish node cache key: %w", err)
	}
	return key, nil
}
