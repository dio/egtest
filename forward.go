package egtest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// Forward owns one kubectl process. Close is idempotent; Cluster.Close also
// closes every forward. URL is ready when PortForward returns successfully.
type Forward struct {
	URL    string
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
}

func (f *Forward) Close() error {
	f.once.Do(func() {
		f.cancel()
		select {
		case <-f.done:
		case <-time.After(3 * time.Second):
			f.err = errors.New("egtest: port-forward did not stop")
		}
	})
	return f.err
}

type forwardOutput struct {
	mu   sync.Mutex
	text string
}

func (b *forwardOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if left := 4096 - len(b.text); left > 0 {
		b.text += string(p[:min(len(p), left)])
	}
	return len(p), nil
}

var forwardPort = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:([0-9]+)`)

func (b *forwardOutput) port() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := forwardPort.FindStringSubmatch(b.text)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// PortForward lets kubectl allocate a loopback port without a reserve/release
// race. ctx bounds startup only; close the returned handle to stop the forward.
func (c *Cluster) PortForward(ctx context.Context, namespace, target string, remotePort int) (*Forward, error) {
	if remotePort < 1 || remotePort > 65535 {
		return nil, errors.New("egtest: invalid remote port")
	}
	call, done, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	call, stop := context.WithTimeout(call, 15*time.Second)
	defer stop()
	life, cancel := context.WithCancel(c.ctx)
	f := &Forward{cancel: cancel, done: make(chan struct{})}
	cmd := exec.CommandContext(life, c.tools["kubectl"], "--kubeconfig", c.info.Kubeconfig, "--context", "k3d-"+c.info.Name, "-n", namespace, "port-forward", "--address=127.0.0.1", target, fmt.Sprintf(":%d", remotePort))
	cmd.WaitDelay = time.Second
	out := &forwardOutput{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("egtest: port-forward start failed")
	}
	go func() { _ = cmd.Wait(); close(f.done) }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-call.Done():
			_ = f.Close()
			return nil, call.Err()
		case <-f.done:
			_ = f.Close()
			return nil, errors.New("egtest: port-forward exited during startup")
		case <-ticker.C:
			if port := out.port(); port != "" {
				c.mu.Lock()
				if c.closed {
					c.mu.Unlock()
					_ = f.Close()
					return nil, ErrClosed
				}
				f.URL = "http://127.0.0.1:" + port
				c.forwards = append(c.forwards, f)
				c.mu.Unlock()
				return f, nil
			}
		}
	}
}
