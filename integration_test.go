package egtest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"text/template"
	"time"

	"github.com/dio/egtest"
)

//go:embed testdata/helm-values.yaml
var helmValues []byte

//go:embed testdata/surface.yaml
var surfaceManifest string

const backendImage = "busybox:1.37.0-musl@sha256:fc6dddc4c44b1bfe37f41cae8e67d1693828e8f42a91862816d7953e2c9d3f23"

type liveReport struct {
	Surface                  egtest.Info `json:"surface"`
	Keep                     egtest.Info `json:"keep"`
	ImageImport              bool        `json:"image_import"`
	DeploymentWait           bool        `json:"deployment_wait"`
	GatewayProgrammed        bool        `json:"gateway_programmed"`
	PatchProgrammed          bool        `json:"patch_programmed"`
	PortForwardConnected     bool        `json:"port_forward_connected"`
	PortForwardClosed        bool        `json:"port_forward_closed"`
	KeepVerified             bool        `json:"keep_verified"`
	RetainedResourcesRemoved bool        `json:"retained_resources_removed"`
	Passed                   bool        `json:"passed"`
}

func TestLive(t *testing.T) {
	if os.Getenv("EGTEST_INTEGRATION") != "1" {
		t.Skip("not run: invoke make integration with explicit version pins")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	opts := egtest.Options{EGVersion: os.Getenv("EGTEST_EG_VERSION"), K3SVersion: os.Getenv("EGTEST_K3S_VERSION"), HelmValues: helmValues}
	report := new(liveReport)
	t.Cleanup(func() {
		report.Passed = !t.Failed()
		if path := os.Getenv("EGTEST_REPORT"); path != "" {
			raw, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
				t.Error(err)
			}
		}
	})
	if !t.Run("surface", func(t *testing.T) { testLiveSurface(t, ctx, opts, report) }) {
		return
	}
	t.Run("keep", func(t *testing.T) { testLiveKeep(t, ctx, opts, report) })
}

func testLiveSurface(t *testing.T, ctx context.Context, opts egtest.Options, report *liveReport) {
	c, err := egtest.Open(ctx, opts)
	if err != nil {
		captureSetupInfo(err, &report.Surface)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
		report.Surface = c.Info()
		if report.Surface.Cleanup != "verified" {
			t.Error("surface cleanup not verified")
		}
	})
	if !c.Info().InstallationVerified {
		t.Fatal("installation not verified")
	}
	if _, err := liveCommand(ctx, "docker", "pull", backendImage); err != nil {
		t.Fatal(err)
	}
	image := c.Info().Name + "-backend:test"
	if _, err := liveCommand(ctx, "docker", "tag", backendImage, image); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := liveCommand(cleanup, "docker", "image", "rm", image); err != nil {
			t.Error(err)
		}
	})
	if err := c.ImportImages(ctx, image); err != nil {
		t.Fatal(err)
	}
	fixture, err := template.New("surface").Parse(surfaceManifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest bytes.Buffer
	if err := fixture.Execute(&manifest, struct{ BackendImage string }{image}); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(ctx, manifest.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Kubectl(ctx, manifest.Bytes(), "apply", "--server-side", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	for _, deployment := range [][2]string{{"envoy-gateway-system", "envoy-gateway"}, {"default", "backend"}} {
		if err := c.WaitDeployment(ctx, deployment[0], deployment[1]); err != nil {
			t.Fatal(err)
		}
	}
	// The unique image tag and Never pull policy make backend readiness proof of import.
	report.ImageImport, report.DeploymentWait = true, true
	if err := c.WaitProgrammed(ctx, "default", "gateway", "smoke"); err != nil {
		t.Fatal(err)
	}
	report.GatewayProgrammed = true
	if err := c.WaitProgrammed(ctx, "default", "envoypatchpolicy", "smoke"); err != nil {
		t.Fatal(err)
	}
	report.PatchProgrammed = true
	var service string
	err = eventuallyLive(ctx, time.Minute, func(ctx context.Context) error {
		raw, err := c.Kubectl(ctx, nil, "-n", "envoy-gateway-system", "get", "services", "-l", "gateway.envoyproxy.io/owning-gateway-namespace=default,gateway.envoyproxy.io/owning-gateway-name=smoke", "-o", "json")
		if err != nil {
			return err
		}
		var list struct {
			Items []struct{ Metadata struct{ Name string } }
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		if len(list.Items) != 1 {
			return fmt.Errorf("expected one owned Envoy service, got %d", len(list.Items))
		}
		service = list.Items[0].Metadata.Name
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.PortForward(ctx, "envoy-gateway-system", "service/"+service, 8080)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	err = eventuallyLive(ctx, 2*time.Minute, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return errors.New("forwarded HTTP request failed")
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 1024))
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusOK || string(body) != "egtest-connected\n" {
			return fmt.Errorf("unexpected fixture response (HTTP %d)", res.StatusCode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	report.PortForwardConnected = true
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", endpoint.Host, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("closed forward still accepts connections")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("could not verify closed forward: dial timed out")
	}
	report.PortForwardClosed = true
}

func testLiveKeep(t *testing.T, ctx context.Context, opts egtest.Options, report *liveReport) {
	opts.Keep = true
	var c *egtest.Cluster
	t.Cleanup(func() {
		if c != nil {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
			report.Keep = c.Info()
		}
		info := report.Keep
		if info.Name == "" || info.Cleanup == "verified" {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := liveCommand(cleanup, "k3d", "cluster", "delete", info.Name); err != nil {
			t.Error(err)
			return
		}
		if err := eventuallyLive(cleanup, time.Minute, func(ctx context.Context) error {
			exists, err := liveClusterExists(ctx, info.Name)
			if err != nil {
				return err
			}
			if exists {
				return errors.New("retained cluster still exists after deletion")
			}
			for _, args := range [][]string{
				{"ps", "-aq", "--filter", "label=k3d.cluster=" + info.Name},
				{"network", "ls", "-q", "--filter", "name=^k3d-" + info.Name + "$"},
				{"volume", "ls", "-q", "--filter", "name=^k3d-" + info.Name + "-images$"},
			} {
				raw, err := liveCommand(ctx, "docker", args...)
				if err != nil {
					return err
				}
				if len(bytes.TrimSpace(raw)) != 0 {
					return errors.New("retained Docker resources remain after deletion")
				}
			}
			return nil
		}); err != nil {
			t.Error(err)
			return
		}
		if info.Kubeconfig != "" {
			if err := os.RemoveAll(filepath.Dir(info.Kubeconfig)); err != nil {
				t.Error(err)
				return
			}
		}
		report.RetainedResourcesRemoved = true
	})
	var err error
	c, err = egtest.Open(ctx, opts)
	if err != nil {
		captureSetupInfo(err, &report.Keep)
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	report.Keep = c.Info()
	if report.Keep.Cleanup != "retained" {
		t.Fatal("Keep did not report retained resources")
	}
	exists, err := liveClusterExists(ctx, report.Keep.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("Keep removed the cluster")
	}
	if _, err := os.Stat(report.Keep.Kubeconfig); err != nil {
		t.Fatal("Keep did not retain kubeconfig")
	}
	report.KeepVerified = true
}

func captureSetupInfo(err error, info *egtest.Info) {
	var setup *egtest.SetupError
	if errors.As(err, &setup) {
		*info = setup.Info
	}
}

func liveClusterExists(ctx context.Context, name string) (bool, error) {
	raw, err := liveCommand(ctx, "k3d", "cluster", "list", "-o", "json")
	if err != nil {
		return false, err
	}
	var clusters []struct{ Name string }
	if err := json.Unmarshal(raw, &clusters); err != nil {
		return false, err
	}
	for _, c := range clusters {
		if c.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func liveCommand(ctx context.Context, tool string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.WaitDelay = 2 * time.Second
	raw, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exit.Stderr = nil
		}
		return nil, fmt.Errorf("%s failed: %w", tool, err)
	}
	return raw, nil
}

func eventuallyLive(ctx context.Context, timeout time.Duration, check func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := check(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
