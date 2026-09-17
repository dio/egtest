// Package egtest owns disposable k3d clusters and Envoy Gateway installations
// for Go integration tests. It never adopts a cluster or changes the caller's
// kubeconfig. Every successful Open must be paired with Close; New registers
// cleanup with the testing package automatically.
package egtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ErrClosed = errors.New("egtest: cluster is closed")

// Options configures one private single-server cluster. Versions are required:
// the consuming project owns qualification of its Kubernetes/EG pairing.
type Options struct {
	EGVersion  string
	K3SVersion string
	// HelmValues is passed on stdin, never logged or persisted by egtest.
	HelmValues []byte
	// Prefix defaults to egtest. A random suffix is always appended.
	Prefix string
	// Timeout bounds setup and each operation (default ten minutes).
	Timeout time.Duration
	// CleanupTimeout is independent of setup cancellation (default two minutes).
	CleanupTimeout time.Duration
	// Keep retains the cluster and private kubeconfig, but stops port forwards.
	// This also applies to failed setup; the returned error identifies resources.
	Keep   bool
	Logger *slog.Logger
}

// Info contains lifecycle evidence, never Kubernetes credentials or raw config.
type Info struct {
	Name                 string `json:"cluster"`
	Kubeconfig           string `json:"kubeconfig"`
	EGVersion            string `json:"envoy_gateway"`
	K3SVersion           string `json:"k3s"`
	InstallationVerified bool   `json:"installation_verified"`
	Cleanup              string `json:"cleanup"`
}

// SetupError carries the cleanup result for setup that allocated private
// resources. Callers can persist Info even when Open did not return a Cluster.
type SetupError struct {
	Info Info
	Err  error
}

func (e *SetupError) Error() string { return "egtest: setup failed: " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

// Cluster owns its resources independently of the context used to open it.
// Methods may run concurrently; Close cancels active operations and is idempotent.
// Stop application workers before Close. Raw Kubectl output belongs to the caller
// and may contain secrets; egtest never logs it.
type Cluster struct {
	opts      Options
	tools     map[string]string
	dir       string
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	info      Info
	attempted bool
	closed    bool
	ops       sync.WaitGroup
	forwards  []*Forward
	closeOnce sync.Once
	closeErr  error
}

// New opens a cluster and registers cleanup for the parent test and all of its
// subtests. Use Open for TestMain or callers that handle errors themselves.
func New(t testing.TB, opts Options) *Cluster {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(testLog{t}, nil))
	}
	c, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

type testLog struct{ t testing.TB }

func (w testLog) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// Open creates a cluster and installs EG. Failed setup performs owned cleanup
// with a fresh deadline before returning an error. No existing cluster is reused.
func Open(ctx context.Context, opts Options) (*Cluster, error) {
	tools := make(map[string]string)
	for _, tool := range []string{"docker", "k3d", "kubectl", "helm"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("egtest: prerequisite %s unavailable", tool)
		}
		tools[tool] = path
	}
	return openWithTools(ctx, opts, tools)
}

func openWithTools(ctx context.Context, opts Options, tools map[string]string) (*Cluster, error) {
	if opts.EGVersion == "" || opts.K3SVersion == "" {
		return nil, errors.New("egtest: explicit EGVersion and K3SVersion are required")
	}
	if opts.Prefix == "" {
		opts.Prefix = "egtest"
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`).MatchString(opts.Prefix) {
		return nil, errors.New("egtest: prefix must be 1–24 lowercase letters, digits or hyphens, starting with a letter")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.CleanupTimeout == 0 {
		opts.CleanupTimeout = 2 * time.Minute
	}
	if opts.Timeout < 0 || opts.CleanupTimeout < 0 {
		return nil, errors.New("egtest: timeouts must be positive")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	name := opts.Prefix + "-" + hex.EncodeToString(id[:])
	dir, err := os.MkdirTemp("", name+"-")
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	c := &Cluster{opts: opts, tools: tools, dir: dir, ctx: life, cancel: cancel,
		info: Info{Name: name, Kubeconfig: filepath.Join(dir, "kubeconfig"), EGVersion: opts.EGVersion, K3SVersion: opts.K3SVersion, Cleanup: "pending"}}
	setup, stop := context.WithTimeout(ctx, opts.Timeout)
	defer stop()
	if err := c.setup(setup); err != nil {
		cleanup := c.Close()
		if c.Info().Cleanup == "retained" {
			err = errors.Join(err, fmt.Errorf("egtest: retained cluster %s; kubeconfig %s; cleanup: k3d cluster delete %s; remove directory %s", name, c.info.Kubeconfig, name, dir))
		}
		return nil, &SetupError{Info: c.Info(), Err: errors.Join(err, cleanup)}
	}
	return c, nil
}

func (c *Cluster) run(ctx context.Context, tool string, input []byte, args ...string) ([]byte, error) {
	return command(ctx, c.tools[tool], tool, input, args...)
}

func (c *Cluster) setup(ctx context.Context) error {
	if _, err := c.run(ctx, "docker", nil, "info", "--format", "{{.Architecture}}"); err != nil {
		return fmt.Errorf("check Docker: %w", err)
	}
	names, err := c.clusterNames(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == c.info.Name {
			return errors.New("egtest: refusing to adopt an existing cluster")
		}
	}
	c.opts.Logger.Info("creating owned EG cluster", "cluster", c.info.Name, "kubeconfig", c.info.Kubeconfig)
	c.attempted = true
	_, err = c.run(ctx, "k3d", nil, "cluster", "create", c.info.Name, "--servers", "1", "--agents", "0", "--api-port", "127.0.0.1:0", "--image", "rancher/k3s:"+c.opts.K3SVersion, "--k3s-arg", "--disable=traefik@server:*", "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--wait", "--timeout", "180s")
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	raw, err := c.run(ctx, "k3d", nil, "kubeconfig", "get", c.info.Name)
	if err != nil {
		return fmt.Errorf("get private kubeconfig: %w", err)
	}
	if err := os.WriteFile(c.info.Kubeconfig, raw, 0o600); err != nil {
		return err
	}
	// k3d v5.8.3 leaves port zero in kubeconfig when Docker allocates the port.
	raw, err = c.run(ctx, "docker", nil, "port", "k3d-"+c.info.Name+"-serverlb", "6443/tcp")
	if err != nil {
		return fmt.Errorf("resolve API port: %w", err)
	}
	endpoint := strings.TrimSpace(string(raw))
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host != "127.0.0.1" || port == "0" || port == "" {
		return errors.New("egtest: owned API must have an allocated loopback port")
	}
	if _, err := c.kube(ctx, nil, "config", "set-cluster", "k3d-"+c.info.Name, "--server=https://"+endpoint); err != nil {
		return err
	}
	if _, err := c.kube(ctx, nil, "wait", "--for=condition=Ready", "nodes/k3d-"+c.info.Name+"-server-0", "--timeout=90s"); err != nil {
		return fmt.Errorf("wait for node: %w", err)
	}
	args := []string{"install", "eg", "oci://docker.io/envoyproxy/gateway-helm", "--version", c.opts.EGVersion, "--kubeconfig", c.info.Kubeconfig, "--kube-context", "k3d-" + c.info.Name, "--namespace", "envoy-gateway-system", "--create-namespace", "--wait", "--timeout", "180s"}
	if len(c.opts.HelmValues) > 0 {
		args = append(args, "--values", "-")
	}
	if _, err := c.run(ctx, "helm", c.opts.HelmValues, args...); err != nil {
		return fmt.Errorf("install Envoy Gateway: %w", err)
	}
	if err := c.verify(ctx); err != nil {
		return err
	}
	c.info.InstallationVerified = true
	return nil
}

func (c *Cluster) clusterNames(ctx context.Context) ([]string, error) {
	raw, err := c.run(ctx, "k3d", nil, "cluster", "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	var values []struct{ Name string }
	if json.Unmarshal(raw, &values) != nil {
		return nil, errors.New("egtest: invalid cluster inventory")
	}
	names := make([]string, len(values))
	for i, v := range values {
		names[i] = v.Name
	}
	return names, nil
}

func (c *Cluster) Info() Info { c.mu.Lock(); defer c.mu.Unlock(); return c.info }

func (c *Cluster) begin(ctx context.Context) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, ErrClosed
	}
	c.ops.Add(1)
	call, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	stop := context.AfterFunc(c.ctx, cancel)
	return call, func() { stop(); cancel(); c.ops.Done() }, nil
}

// Close stops forwards, removes the exact owned cluster, verifies Docker
// resource absence, then removes private files. Failures retain their paths.
// Keep stops forwards but deliberately retains the cluster and kubeconfig.
func (c *Cluster) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.cancel()
		c.mu.Unlock()
		c.ops.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), c.opts.CleanupTimeout)
		defer cancel()
		for _, f := range c.forwards {
			c.closeErr = errors.Join(c.closeErr, f.Close())
		}
		cleanup := "verified"
		if c.opts.Keep && c.attempted {
			cleanup = "retained"
			c.opts.Logger.Info("retained EG resources", "cluster", c.info.Name, "kubeconfig", c.info.Kubeconfig, "cleanup_cluster", "k3d cluster delete "+c.info.Name, "cleanup_directory", c.dir)
		} else {
			c.closeErr = errors.Join(c.closeErr, c.remove(ctx))
		}
		if c.closeErr != nil {
			cleanup = "failed"
			c.closeErr = fmt.Errorf("egtest: cleanup for cluster %s (private files %s): %w", c.info.Name, c.dir, c.closeErr)
		}
		c.mu.Lock()
		c.info.Cleanup = cleanup
		c.mu.Unlock()
	})
	return c.closeErr
}

func (c *Cluster) remove(ctx context.Context) error {
	if c.attempted {
		if _, err := c.run(ctx, "k3d", nil, "cluster", "delete", c.info.Name); err != nil {
			return err
		}
		names, err := c.clusterNames(ctx)
		if err != nil {
			return err
		}
		for _, name := range names {
			if name == c.info.Name {
				return errors.New("owned cluster still exists")
			}
		}
		for _, args := range [][]string{
			{"ps", "-aq", "--filter", "label=k3d.cluster=" + c.info.Name},
			{"network", "ls", "-q", "--filter", "name=^k3d-" + c.info.Name + "$"},
			{"volume", "ls", "-q", "--filter", "name=^k3d-" + c.info.Name + "-images$"},
		} {
			raw, err := c.run(ctx, "docker", nil, args...)
			if err != nil {
				return err
			}
			if len(bytes.TrimSpace(raw)) != 0 {
				return errors.New("owned Docker resources remain")
			}
		}
	}
	return os.RemoveAll(c.dir)
}
