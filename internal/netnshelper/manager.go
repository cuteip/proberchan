package netnshelper

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/vishvananda/netns"
	"go.uber.org/zap"
)

type Executor interface {
	WithNamespace(ctx context.Context, namespace string, fn func(context.Context) error) error
}

type Manager struct {
	logger  *zap.Logger
	mu      sync.RWMutex
	handles map[string]netns.NsHandle
}

func NewManager(logger *zap.Logger) *Manager {
	return &Manager{
		logger:  logger,
		handles: make(map[string]netns.NsHandle),
	}
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for name, handle := range m.handles {
		if err := handle.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.handles, name)
	}
	return firstErr
}

func (m *Manager) WithNamespace(ctx context.Context, namespace string, fn func(context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("nil namespace function")
	}
	if m == nil {
		return fn(ctx)
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return fn(ctx)
	}

	handle, err := m.getHandle(namespace)
	if err != nil {
		return err
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to snapshot current network namespace: %w", err)
	}
	defer func() {
		if err := orig.Close(); err != nil {
			m.logger.Warn("failed to close original network namespace handle", zap.Error(err))
		}
	}()

	if err := netns.Set(handle); err != nil {
		m.invalidate(namespace)
		return fmt.Errorf("failed to switch to netns %q: %w", namespace, err)
	}
	defer func() {
		if err := netns.Set(orig); err != nil {
			m.logger.Warn("failed to restore original network namespace", zap.String("netns", namespace), zap.Error(err))
		}
	}()

	return fn(ctx)
}

func (m *Manager) getHandle(namespace string) (netns.NsHandle, error) {
	m.mu.RLock()
	handle, ok := m.handles[namespace]
	m.mu.RUnlock()
	if ok {
		return handle, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if handle, ok := m.handles[namespace]; ok {
		return handle, nil
	}

	nsHandle, err := netns.GetFromName(namespace)
	if err != nil {
		return 0, fmt.Errorf("failed to open netns %q: %w", namespace, err)
	}
	m.handles[namespace] = nsHandle
	return nsHandle, nil
}

func (m *Manager) invalidate(namespace string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handle, ok := m.handles[namespace]; ok {
		if err := handle.Close(); err != nil {
			m.logger.Warn("failed to close stale namespace handle", zap.String("netns", namespace), zap.Error(err))
		}
		delete(m.handles, namespace)
	}
}
