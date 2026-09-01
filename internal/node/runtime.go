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

	"asterferry/internal/atomicfile"
	controlwire "asterferry/internal/controlwire"
	v1 "asterferry/internal/controlwire/v1"
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
	runtimeMu     sync.RWMutex
	engine        *dataplane.Engine
	dataPlane     *DataPlaneRuntime
	runtimeKind   string
	runtimeOpts   RuntimeOptions
	runCtx        context.Context
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
	runtime := &Runtime{bootstrap: bootstrap, runtimeOpts: options, logger: options.Logger, bootstrapPath: options.BootstrapPath}
	apply := func(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
		return runtime.applySnapshot(ctx, snapshot, previous)
	}
	reset := func(ctx context.Context, generation uint64) error {
		return runtime.resetSnapshot(ctx, generation)
	}
	reconciler, err := NewReconcilerWithReset(cache, apply, reset)
	if err != nil {
		return nil, err
	}
	runtime.reconciler = reconciler
	if bootstrap.Role != "" {
		if _, _, err := runtime.ensureRuntimeKind(bootstrap.Role); err != nil {
			return nil, err
		}
	}
	if snapshot, readErr := cache.Read(); readErr == nil {
		if kind, kindErr := snapshotRuntimeKind(snapshot); kindErr != nil {
			return nil, kindErr
		} else if kind != "" {
			engine, dataPlane, ensureErr := runtime.ensureRuntime(snapshot)
			if ensureErr != nil {
				return nil, ensureErr
			}
			if applyErr := engine.ApplySnapshot(context.Background(), snapshot, nil); applyErr != nil {
				return nil, fmt.Errorf("apply cached snapshot: %w", applyErr)
			}
			if applyErr := dataPlane.ApplySnapshot(context.Background(), snapshot, nil); applyErr != nil {
				return nil, fmt.Errorf("prepare cached data-plane snapshot: %w", applyErr)
			}
		}
	}
	if err := reconciler.SetNodeID(bootstrap.NodeID); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *Runtime) Engine() *dataplane.Engine {
	r.runtimeMu.RLock()
	defer r.runtimeMu.RUnlock()
	return r.engine
}

func (r *Runtime) DataPlane() *DataPlaneRuntime {
	r.runtimeMu.RLock()
	defer r.runtimeMu.RUnlock()
	return r.dataPlane
}

func (r *Runtime) Reconciler() *Reconciler { return r.reconciler }

func snapshotRuntimeKind(snapshot domain.DesiredSnapshot) (string, error) {
	if snapshot.Gateway != nil && snapshot.Agent == nil {
		return domain.RoleGateway, nil
	}
	if snapshot.Agent != nil && snapshot.Gateway == nil {
		return domain.RoleAgent, nil
	}
	if snapshot.Gateway == nil && snapshot.Agent == nil {
		return "", nil
	}
	return "", errors.New("desired snapshot selects multiple node behaviors")
}

// ensureRuntimeKind lazily creates the role-specific data-plane components.
// The process, certificate, control stream, cache, and reconnect loop are
// shared; only the adapters behind the current spec are replaced.
func (r *Runtime) ensureRuntimeKind(kind string) (*dataplane.Engine, *DataPlaneRuntime, error) {
	if kind != domain.RoleGateway && kind != domain.RoleAgent {
		return nil, nil, errors.New("node behavior must be gateway or agent")
	}
	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()
	if r.engine != nil && r.runtimeKind == kind {
		return r.engine, r.dataPlane, nil
	}
	oldEngine, oldDataPlane := r.engine, r.dataPlane
	if oldDataPlane != nil {
		_ = oldDataPlane.Close()
	}
	if oldEngine != nil {
		_ = oldEngine.Close()
	}
	// Do not leave closed components installed while constructing the
	// replacement. A transient constructor or Start failure must be safe to
	// retry instead of reusing a data plane that has already been closed.
	r.engine, r.dataPlane, r.runtimeKind = nil, nil, ""
	engine, err := dataplane.New(dataplane.Options{Role: kind, NodeID: r.bootstrap.NodeID, MaxStreams: r.runtimeOpts.MaxStreams, MaxSessions: r.runtimeOpts.MaxSessions})
	if err != nil {
		return nil, nil, err
	}
	dataPlane, err := NewDataPlaneRuntime(DataPlaneOptions{Engine: engine, Bootstrap: r.bootstrapSnapshot(), Logger: r.runtimeOpts.Logger})
	if err != nil {
		_ = engine.Close()
		return nil, nil, err
	}
	r.engine, r.dataPlane, r.runtimeKind = engine, dataPlane, kind
	if r.runCtx != nil {
		if err := dataPlane.Start(r.runCtx); err != nil {
			r.engine, r.dataPlane, r.runtimeKind = nil, nil, ""
			_ = dataPlane.Close()
			_ = engine.Close()
			return nil, nil, err
		}
	}
	return engine, dataPlane, nil
}

func (r *Runtime) ensureRuntime(snapshot domain.DesiredSnapshot) (*dataplane.Engine, *DataPlaneRuntime, error) {
	kind, err := snapshotRuntimeKind(snapshot)
	if err != nil {
		return nil, nil, err
	}
	if kind == "" {
		return nil, nil, r.disableRuntime()
	}
	return r.ensureRuntimeKind(kind)
}

// disableRuntime is the explicit empty-spec transition. Clear the published
// pointers before closing sockets so observers never receive a closed runtime
// as if it were still active.
func (r *Runtime) disableRuntime() error {
	r.runtimeMu.Lock()
	oldEngine, oldDataPlane := r.engine, r.dataPlane
	r.engine, r.dataPlane, r.runtimeKind = nil, nil, ""
	r.runtimeMu.Unlock()
	var closeErrs []error
	if oldDataPlane != nil {
		if err := oldDataPlane.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	if oldEngine != nil {
		if err := oldEngine.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	return errors.Join(closeErrs...)
}

func (r *Runtime) applySnapshot(ctx context.Context, snapshot domain.DesiredSnapshot, previous *domain.DesiredSnapshot) error {
	kind, err := snapshotRuntimeKind(snapshot)
	if err != nil {
		return err
	}
	previousKind := ""
	if previous != nil {
		previousKind, _ = snapshotRuntimeKind(*previous)
	}
	if kind == "" {
		return r.disableRuntime()
	}
	engine, dataPlane, err := r.ensureRuntime(snapshot)
	if err != nil {
		if previous != nil && previousKind != "" && previousKind != kind {
			restoreErr := r.restoreRuntime(ctx, *previous)
			return errors.Join(err, restoreErr)
		}
		return err
	}
	sameKind := previous == nil || previousKind == kind
	enginePrevious := previous
	if !sameKind {
		// An engine rejects a snapshot for the other behavior. A kind switch is
		// a complete component replacement, so the new engine starts without a
		// role-mismatched rollback baseline.
		enginePrevious = nil
	}
	wasDraining := engine.IsDraining()
	engine.BeginDrain()
	if err := engine.ApplySnapshot(ctx, snapshot, enginePrevious); err != nil {
		if previous != nil && !sameKind {
			restoreErr := r.restoreRuntime(ctx, *previous)
			return errors.Join(err, restoreErr)
		}
		return err
	}
	if err := dataPlane.ApplySnapshot(ctx, snapshot, enginePrevious); err != nil {
		r.logger.Warn("data-plane snapshot apply failed", "generation", snapshot.Generation, "error", err)
		rollbackCtx := context.WithoutCancel(ctx)
		if previous == nil {
			return errors.Join(err, engine.ResetSnapshot(snapshot.Generation))
		}
		if sameKind {
			var rollbackErrs []error
			if rollbackErr := engine.ApplySnapshot(rollbackCtx, *previous, &snapshot); rollbackErr != nil {
				rollbackErrs = append(rollbackErrs, rollbackErr)
			}
			if rollbackErr := dataPlane.ApplySnapshot(rollbackCtx, *previous, nil); rollbackErr != nil {
				rollbackErrs = append(rollbackErrs, rollbackErr)
			}
			if len(rollbackErrs) > 0 {
				return errors.Join(err, errors.Join(rollbackErrs...))
			}
			if !wasDraining {
				engine.EndDrain()
			}
			return err
		}
		// A failed kind switch is restored by constructing the old role again.
		oldEngine, oldDataPlane, restoreErr := r.ensureRuntimeKind(previousKind)
		if restoreErr == nil {
			restoreErr = oldEngine.ApplySnapshot(rollbackCtx, *previous, nil)
		}
		if restoreErr == nil {
			restoreErr = oldDataPlane.ApplySnapshot(rollbackCtx, *previous, nil)
		}
		return errors.Join(err, restoreErr)
	}
	if !wasDraining {
		engine.EndDrain()
	}
	return nil
}

func (r *Runtime) restoreRuntime(ctx context.Context, snapshot domain.DesiredSnapshot) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rollbackCtx := context.WithoutCancel(ctx)
	kind, err := snapshotRuntimeKind(snapshot)
	if err != nil {
		return err
	}
	if kind == "" {
		return r.disableRuntime()
	}
	engine, dataPlane, err := r.ensureRuntimeKind(kind)
	if err != nil {
		return err
	}
	if err := engine.ApplySnapshot(rollbackCtx, snapshot, nil); err != nil {
		return err
	}
	return dataPlane.ApplySnapshot(rollbackCtx, snapshot, nil)
}

func (r *Runtime) resetSnapshot(ctx context.Context, generation uint64) error {
	engine, dataPlane := r.Engine(), r.DataPlane()
	if engine == nil || dataPlane == nil {
		return nil
	}
	return errors.Join(dataPlane.ResetSnapshot(generation), engine.ResetSnapshot(generation))
}

func (r *Runtime) observedState() domain.ObservedState {
	observed := r.reconciler.State()
	dataPlane := r.DataPlane()
	if dataPlane == nil {
		return observed
	}
	dataObserved, ok := dataPlane.ObservedState()
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
		if !waitWithContext(ctx, backoff) {
			return nil
		}
	}
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
	role := v1.NodeRole_NODE_ROLE_UNSPECIFIED
	if engine := r.Engine(); engine != nil {
		if engine.Role() == domain.RoleGateway {
			role = v1.NodeRole_NODE_ROLE_GATEWAY
		} else if engine.Role() == domain.RoleAgent {
			role = v1.NodeRole_NODE_ROLE_AGENT
		}
	} else if bootstrap.Role == domain.RoleGateway {
		role = v1.NodeRole_NODE_ROLE_GATEWAY
	} else if bootstrap.Role == domain.RoleAgent {
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
	if observed, observedErr := controlwire.ObservedToProto(state); observedErr == nil {
		if err := send(&v1.NodeMessage{Body: &v1.NodeMessage_ObservedState{ObservedState: observed}}); err != nil {
			return err
		}
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(connectionCtx)
	defer cancelHeartbeat()
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

func renewalRequest(bootstrap Bootstrap) ([]byte, error) {
	certificate, err := tls.X509KeyPair([]byte(bootstrap.CertificatePEM), []byte(bootstrap.PrivateKeyPEM))
	if err != nil {
		return nil, err
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("node certificate is empty")
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
		return errors.New("controller returned an empty certificate bundle")
	}
	leaf, err := x509.ParseCertificate(bundle.CertificateDer)
	if err != nil {
		return fmt.Errorf("controller returned an invalid certificate: %w", err)
	}
	if strings.TrimSpace(bundle.Serial) == "" || !strings.EqualFold(leaf.SerialNumber.Text(16), strings.TrimSpace(bundle.Serial)) {
		return errors.New("controller certificate serial does not match the bundle")
	}
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("controller certificate is expired or not yet valid")
	}
	bootstrap := r.bootstrapSnapshot()
	if leaf.Subject.CommonName != bootstrap.NodeID {
		return errors.New("controller certificate identity does not match the node")
	}
	keyPair, err := tls.X509KeyPair([]byte(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CertificateDer})), []byte(bootstrap.PrivateKeyPEM))
	if err != nil || len(keyPair.Certificate) == 0 {
		return errors.New("controller certificate does not match the node private key")
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CertificateDer})
	caPEM := []byte(bootstrap.CAPEM)
	if len(bundle.CaCertificateDer) > 0 {
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundle.CaCertificateDer})
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("controller certificate bundle has no valid CA")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: now}); err != nil {
		return fmt.Errorf("controller certificate is not signed by the configured CA: %w", err)
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
	dataPlane := r.DataPlane()
	if dataPlane != nil {
		if err := dataPlane.UpdateBootstrap(updated); err != nil {
			var rollbackErrs []error
			if r.bootstrapPath != "" {
				if rollbackErr := WriteBootstrap(r.bootstrapPath, bootstrap); rollbackErr != nil {
					r.logger.Error("node bootstrap rollback failed", "error", rollbackErr)
					rollbackErrs = append(rollbackErrs, rollbackErr)
				}
			}
			if rollbackErr := dataPlane.UpdateBootstrap(bootstrap); rollbackErr != nil {
				r.logger.Error("data-plane bootstrap rollback failed", "error", rollbackErr)
				rollbackErrs = append(rollbackErrs, rollbackErr)
			}
			return errors.Join(err, errors.Join(rollbackErrs...))
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
	tmpPath, err := atomicfile.WriteTemp(path, ".node-key-*", key, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmpPath) }()
	if err := os.Rename(tmpPath, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && len(existing) == 32 {
			return existing, nil
		}
		return nil, fmt.Errorf("publish node cache key: %w", err)
	}
	return key, nil
}
