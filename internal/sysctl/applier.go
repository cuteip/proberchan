package sysctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cuteip/proberchan/internal/netnshelper"
)

const (
	pingGroupRangeKey   = "net.ipv4.ping_group_range"
	pingGroupRangeValue = "0 2147483647"
)

type Applier struct {
	executor netnshelper.Executor
}

func NewApplier(executor netnshelper.Executor) *Applier {
	return &Applier{executor: executor}
}

func (a *Applier) EnsurePingGroupRange(ctx context.Context, namespace string) error {
	setting := []sysctlSetting{{
		key:   pingGroupRangeKey,
		value: pingGroupRangeValue,
	}}
	return a.apply(ctx, namespace, setting)
}

func (a *Applier) apply(ctx context.Context, namespace string, settings []sysctlSetting) error {
	if len(settings) == 0 {
		return nil
	}

	applyFn := func(context.Context) error {
		var errs []error
		for _, s := range settings {
			if err := writeSysctl(s.key, s.value); err != nil {
				errs = append(errs, fmt.Errorf("sysctl %q: %w", s.key, err))
			}
		}
		return errors.Join(errs...)
	}

	if namespace == "" || a.executor == nil {
		return applyFn(ctx)
	}

	return a.executor.WithNamespace(ctx, namespace, applyFn)
}

type sysctlSetting struct {
	key   string
	value string
}

func writeSysctl(key, value string) error {
	path, err := sysctlPath(key)
	if err != nil {
		return err
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("empty value for %q", key)
	}

	data := []byte(trimmed)
	if !strings.HasSuffix(trimmed, "\n") {
		data = append(data, '\n')
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write %q to %s: %w", trimmed, path, err)
	}

	return nil
}

func sysctlPath(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("empty sysctl key")
	}

	parts := strings.Split(key, ".")
	for i := range parts {
		part := strings.TrimSpace(parts[i])
		if part == "" {
			return "", fmt.Errorf("invalid sysctl key %q", key)
		}
		parts[i] = part
	}

	return filepath.Join(append([]string{"/proc/sys"}, parts...)...), nil
}
