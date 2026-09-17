package egtest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run real child processes, but never invoke Docker or Kubernetes.
func fakeTools(t *testing.T) (map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	body := `base='FAKE_ROOT'
tool=${0##*/}
printf '%s\n' "$tool $*" >> "$base/commands"
case "$tool" in
docker)
 case "$1" in
 info) printf arm64 ;;
 port) printf '127.0.0.1:65432\n' ;;
 ps) if [ -f "$base/leftover" ]; then printf leftover; fi ;;
 esac ;;
k3d)
 case "$1 $2" in
 'cluster list')
  if [ -f "$base/state" ]; then name=$(/bin/cat "$base/state"); printf '[{"name":"%s"},{"name":"unrelated"}]' "$name"; else printf '[{"name":"unrelated"}]'; fi ;;
 'cluster create')
  printf '%s' "$3" > "$base/state"
  if [ -f "$base/fail-create" ]; then printf 'private-error' >&2; exit 7; fi ;;
 'cluster delete')
  name=$(/bin/cat "$base/state")
  [ "$3" = "$name" ] || exit 9
  if [ -f "$base/fail-delete" ]; then exit 8; fi
  /bin/rm "$base/state"; printf '%s' "$3" > "$base/deleted" ;;
 'kubeconfig get') printf private-kubeconfig ;;
 'image import') : ;;
 *) exit 9 ;;
 esac ;;
helm)
 /bin/cat >/dev/null
 if [ -f "$base/fail-helm" ]; then printf 'private-error' >&2; exit 7; fi ;;
kubectl)
 [ "$1" = --kubeconfig ] && [ "$3" = --context ] || exit 9
 printf '%s' "$2" > "$base/kubeconfig"
 name=$(/bin/cat "$base/state")
 [ "$4" = "k3d-$name" ] || exit 9
 shift 4
 if [ "$1" = -n ]; then shift 2; fi
 case "$1 $2" in
 'version -o') printf '{"serverVersion":{"gitVersion":"v1.33.13+k3s2"}}' ;;
 'get nodes') printf '{"items":[{}]}' ;;
 'get deployment/envoy-gateway') printf '{"spec":{"template":{"spec":{"containers":[{"name":"envoy-gateway","image":"envoyproxy/gateway:v1.9.1"}]}}}}' ;;
 'get deployment/traefik') : ;;
 'apply -f') /bin/cat >/dev/null ;;
 'port-forward --address=127.0.0.1')
  if [ -f "$base/fail-forward" ]; then printf private-error >&2; exit 7; fi
  printf 'Forwarding from 127.0.0.1:45678 -> 8080\n'
  exec /bin/sleep 600 ;;
 'block '*) printf started > "$base/blocked"; exec /bin/sleep 600 ;;
 esac ;;
*) exit 9 ;;
esac
`
	body = strings.ReplaceAll(body, "FAKE_ROOT", strings.ReplaceAll(dir, "'", "'\"'\"'"))
	tools := map[string]string{}
	for _, tool := range []string{"docker", "k3d", "kubectl", "helm"} {
		path := filepath.Join(dir, tool)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
		tools[tool] = path
	}
	return tools, dir
}

func options() Options {
	return Options{EGVersion: "v1.9.1", K3SVersion: "v1.33.13-k3s2", Timeout: 5 * time.Second, CleanupTimeout: 5 * time.Second}
}
func marker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLifecycleIsolationAndConcurrentClose(t *testing.T) {
	tools, dir := fakeTools(t)
	ctx, cancel := context.WithCancel(t.Context())
	c, err := openWithTools(ctx, options(), tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	info := c.Info()
	if !info.InstallationVerified {
		t.Fatal("installation was not verified")
	}
	stat, err := os.Stat(info.Kubeconfig)
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig permissions: %v", err)
	}
	stat, err = os.Stat(filepath.Dir(info.Kubeconfig))
	if err != nil || stat.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions: %v", err)
	}
	cancel() // The setup context must not own the running cluster.
	if err := c.Apply(t.Context(), []byte("private-manifest")); err != nil {
		t.Fatal(err)
	}
	if err := c.ImportImages(t.Context(), "local-image:test"); err != nil {
		t.Fatal(err)
	}
	f, err := c.PortForward(t.Context(), "default", "service/example", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if f.URL != "http://127.0.0.1:45678" {
		t.Fatal(f.URL)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Info().Cleanup != "verified" {
		t.Fatal(c.Info())
	}
	if _, err := os.Stat(info.Kubeconfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private kubeconfig remains")
	}
	if _, err := c.Kubectl(t.Context(), nil, "get", "pods"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	commands := read(t, filepath.Join(dir, "commands"))
	if strings.Count(commands, "cluster delete ") != 1 || strings.Contains(commands, "delete unrelated") {
		t.Fatal("unexpected deletion")
	}
	if strings.Contains(commands, "private-manifest") {
		t.Fatal("stdin leaked into command arguments")
	}
}

func TestFailedSetupCleansPartialCluster(t *testing.T) {
	for _, stage := range []string{"fail-create", "fail-helm"} {
		t.Run(stage, func(t *testing.T) {
			tools, dir := fakeTools(t)
			marker(t, dir, stage)
			c, err := openWithTools(t.Context(), options(), tools)
			if err == nil || c != nil {
				t.Fatal("expected setup failure")
			}
			if strings.Contains(err.Error(), "private-error") {
				t.Fatal("stderr leaked")
			}
			var setup *SetupError
			if !errors.As(err, &setup) || setup.Info.Cleanup != "verified" || setup.Info.Name == "" {
				t.Fatal("missing structured setup/cleanup evidence")
			}
			if _, err := os.Stat(filepath.Dir(setup.Info.Kubeconfig)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial setup private directory remains")
			}
			var commandErr *CommandError
			if !errors.As(err, &commandErr) {
				t.Fatal("lost command error")
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || len(exit.Stderr) != 0 {
				t.Fatal("unsafe exit error")
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial cluster remains")
			}
			if stage == "fail-helm" {
				path := read(t, filepath.Join(dir, "kubeconfig"))
				if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("private files remain")
				}
			}
		})
	}
}

func TestKeepAndFailedCleanupRetainInventory(t *testing.T) {
	for _, mode := range []string{"keep", "leftover", "fail-delete"} {
		t.Run(mode, func(t *testing.T) {
			tools, dir := fakeTools(t)
			opts := options()
			opts.Keep = mode == "keep"
			c, err := openWithTools(t.Context(), opts, tools)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(c.dir) })
			if mode != "keep" {
				marker(t, dir, mode)
			}
			err = c.Close()
			if mode == "keep" {
				if err != nil || c.Info().Cleanup != "retained" {
					t.Fatalf("%v %+v", err, c.Info())
				}
				if strings.Contains(read(t, filepath.Join(dir, "commands")), "cluster delete") {
					t.Fatal("deleted retained cluster")
				}
			} else if err == nil || c.Info().Cleanup != "failed" {
				t.Fatalf("missing cleanup failure: %v", err)
			}
			if _, err := os.Stat(c.Info().Kubeconfig); err != nil {
				t.Fatal("lost recovery inventory", err)
			}
		})
	}
}

func TestCloseCancelsActiveCommand(t *testing.T) {
	tools, dir := fakeTools(t)
	c, err := openWithTools(t.Context(), options(), tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	done := make(chan error, 1)
	go func() { _, err := c.Kubectl(context.Background(), nil, "block"); done <- err }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := poll(ctx, func() error { _, err := os.Stat(filepath.Join(dir, "blocked")); return err }); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestPortForwardEarlyFailure(t *testing.T) {
	tools, dir := fakeTools(t)
	c, err := openWithTools(t.Context(), options(), tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	marker(t, dir, "fail-forward")
	start := time.Now()
	_, err = c.PortForward(t.Context(), "default", "service/example", 8080)
	if err == nil || strings.Contains(err.Error(), "private-error") || time.Since(start) > time.Second {
		t.Fatal("forward failure not safely detected", err)
	}
}

func TestCancelledSetupCreatesNothing(t *testing.T) {
	tools, dir := fakeTools(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := openWithTools(ctx, options(), tools); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "commands")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled setup ran a command")
	}
}

func TestKubectlRejectsConnectionOverrides(t *testing.T) {
	tools, _ := fakeTools(t)
	c, err := openWithTools(t.Context(), options(), tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, flag := range []string{"--context=other", "--kubeconfig=other", "--server=https://elsewhere", "--cluster=other", "-shttps://elsewhere", "--insecure-skip-tls-verify"} {
		if _, err := c.Kubectl(t.Context(), nil, "get", "pods", flag); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
	// Flags after the exec separator belong to the remote command.
	if _, err := c.Kubectl(t.Context(), nil, "exec", "pod/example", "--", "program", "--context=application"); err != nil {
		t.Fatal(err)
	}
}

func TestFailedSetupKeepReportsRetainedResources(t *testing.T) {
	tools, dir := fakeTools(t)
	marker(t, dir, "fail-helm")
	opts := options()
	opts.Keep = true
	_, err := openWithTools(t.Context(), opts, tools)
	var setup *SetupError
	if !errors.As(err, &setup) || setup.Info.Cleanup != "retained" {
		t.Fatal("missing retained ownership", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(setup.Info.Kubeconfig)) })
	if !strings.Contains(err.Error(), setup.Info.Name) || !strings.Contains(err.Error(), setup.Info.Kubeconfig) {
		t.Fatal("missing cleanup instructions")
	}
	if strings.Contains(read(t, filepath.Join(dir, "commands")), "cluster delete") {
		t.Fatal("deleted debug cluster")
	}
}

func TestPolicyGenerationAndAncestors(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		policy, want bool
	}{
		{"gateway", `{"conditions":[{"type":"Accepted","status":"True","observedGeneration":2},{"type":"Programmed","status":"True","observedGeneration":2}]}`, false, true},
		{"old-generation", `{"conditions":[{"type":"Accepted","status":"True","observedGeneration":1},{"type":"Programmed","status":"True","observedGeneration":1}]}`, false, false},
		{"nested", `{"ancestors":[{"conditions":[{"type":"Accepted","status":"True","observedGeneration":2},{"type":"Programmed","status":"True","observedGeneration":2}]}]}`, true, true},
		{"different-ancestors", `{"ancestors":[{"conditions":[{"type":"Accepted","status":"True","observedGeneration":2}]},{"conditions":[{"type":"Programmed","status":"True","observedGeneration":2}]}]}`, true, false},
		{"missing", `{}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := programmed([]byte(`{"metadata":{"generation":2},"status":`+tc.status+`}`), tc.policy); got != tc.want {
				t.Fatal(got)
			}
		})
	}
}
