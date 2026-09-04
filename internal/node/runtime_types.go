package node

import (
	"asterferry/internal/dataplane"
	"asterferry/internal/domain"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

type RuntimeOptions struct {
	Logger            *slog.Logger
	CachePath         string
	CacheKeyPath      string
	BootstrapPath     string
	MaxStreams        int
	MaxSessions       int
	GeoIPDatabasePath string
	GeoIPMaxAge       time.Duration
}

type Runtime struct {
	bootstrap     Bootstrap
	bootstrapMu   sync.RWMutex
	runtimeMu     sync.RWMutex
	engine        *dataplane.Engine
	dataPlane     *DataPlaneRuntime
	runtimeKind   domain.NodeSpecKind
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

func snapshotRuntimeKind(snapshot domain.DesiredSnapshot) (domain.NodeSpecKind, error) {
	if snapshot.Gateway != nil && snapshot.Agent == nil {
		return domain.NodeSpecGateway, nil
	}
	if snapshot.Agent != nil && snapshot.Gateway == nil {
		return domain.NodeSpecAgent, nil
	}
	if snapshot.Gateway == nil && snapshot.Agent == nil {
		return "", nil
	}
	return "", errors.New("desired snapshot selects multiple node behaviors")
}

// ensureRuntimeKind lazily creates the data-plane components for the selected
// NodeSpec behavior.
// The process, certificate, control stream, cache, and reconnect loop are
// shared; only the adapters behind the current spec are replaced.
func (r *Runtime) ensureRuntimeKind(kind domain.NodeSpecKind) (*dataplane.Engine, *DataPlaneRuntime, error) {
	if kind != domain.NodeSpecGateway && kind != domain.NodeSpecAgent {
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
	engine, err := dataplane.New(dataplane.Options{Kind: kind, NodeID: r.bootstrap.NodeID, MaxStreams: r.runtimeOpts.MaxStreams, MaxSessions: r.runtimeOpts.MaxSessions})
	if err != nil {
		return nil, nil, err
	}
	dataPlane, err := NewDataPlaneRuntime(DataPlaneOptions{
		Engine:            engine,
		Bootstrap:         r.bootstrapSnapshot(),
		Logger:            r.runtimeOpts.Logger,
		GeoIPDatabasePath: r.runtimeOpts.GeoIPDatabasePath,
		GeoIPMaxAge:       r.runtimeOpts.GeoIPMaxAge,
	})
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
