package egtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (c *Cluster) kube(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	scoped := []string{"--kubeconfig", c.info.Kubeconfig, "--context", "k3d-" + c.info.Name}
	return c.run(ctx, "kubectl", input, append(scoped, args...)...)
}

// Kubectl runs against this cluster's private kubeconfig. The caller owns the
// returned bytes and must avoid logging Secret or raw Envoy configuration data.
// Kubeconfig, context, cluster, server and TLS-verification overrides are rejected.
// Authentication/impersonation flags remain caller-controlled.
func (c *Cluster) Kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		for _, flag := range []string{"--kubeconfig", "--context", "--cluster", "--server", "--insecure-skip-tls-verify"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return nil, errors.New("egtest: kubectl cluster selection and TLS-verification overrides are not allowed")
			}
		}
		// kubectl accepts -s URL, -s=URL and -sURL.
		if strings.HasPrefix(arg, "-s") {
			return nil, errors.New("egtest: kubectl server override is not allowed")
		}
	}
	if len(args) > 0 && args[0] == "config" {
		return nil, errors.New("egtest: kubectl config mutations are not supported")
	}
	call, done, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.kube(call, input, args...)
}

// Apply submits a caller-owned JSON or YAML manifest without writing it to disk.
func (c *Cluster) Apply(ctx context.Context, manifest []byte) error {
	_, err := c.Kubectl(ctx, manifest, "apply", "-f", "-")
	return err
}

// ImportImages imports existing local images. Building and deleting host images
// remain the caller's responsibility; egtest never pushes to a registry.
func (c *Cluster) ImportImages(ctx context.Context, images ...string) error {
	if len(images) == 0 {
		return errors.New("egtest: at least one image is required")
	}
	for _, name := range images {
		if name == "" || strings.HasPrefix(name, "-") {
			return errors.New("egtest: invalid image name")
		}
	}
	call, done, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	_, err = c.run(call, "k3d", nil, append([]string{"image", "import", "-c", c.info.Name}, images...)...)
	return err
}

func (c *Cluster) WaitDeployment(ctx context.Context, namespace, name string) error {
	_, err := c.Kubectl(ctx, nil, "-n", namespace, "rollout", "status", "deployment/"+name, "--timeout=120s")
	return err
}

// WaitProgrammed waits for current-generation Accepted and Programmed conditions
// on a Gateway or EnvoyPatchPolicy. All reported policy ancestors must agree.
// This verifies controller status, not acceptance of xDS by a running Envoy.
func (c *Cluster) WaitProgrammed(ctx context.Context, namespace, kind, name string) error {
	if kind != "gateway" && kind != "envoypatchpolicy" {
		return errors.New("egtest: supported kinds are gateway and envoypatchpolicy")
	}
	call, done, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	call, cancel := context.WithTimeout(call, 2*time.Minute)
	defer cancel()
	return poll(call, func() error {
		raw, err := c.kube(call, nil, "-n", namespace, "get", kind+"/"+name, "-o", "json")
		if err != nil {
			return err
		}
		if !programmed(raw, kind == "envoypatchpolicy") {
			return errors.New("current-generation acceptance and programming not observed")
		}
		return nil
	})
}

func poll(ctx context.Context, check func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := check()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), err)
		case <-timer.C:
		}
	}
}

type condition struct {
	Type, Status       string
	ObservedGeneration int64
}

func programmed(raw []byte, policy bool) bool {
	var v struct {
		Metadata struct{ Generation int64 }
		Status   struct {
			Conditions []condition
			Ancestors  []struct{ Conditions []condition }
		}
	}
	if json.Unmarshal(raw, &v) != nil || v.Metadata.Generation < 1 {
		return false
	}
	ready := func(conditions []condition) bool {
		accepted, programmed := false, false
		for _, c := range conditions {
			if c.Status != "True" || c.ObservedGeneration != v.Metadata.Generation {
				continue
			}
			if c.Type == "Accepted" {
				accepted = true
			}
			if c.Type == "Programmed" {
				programmed = true
			}
		}
		return accepted && programmed
	}
	if !policy {
		return ready(v.Status.Conditions)
	}
	if len(v.Status.Ancestors) == 0 {
		return false
	}
	for _, ancestor := range v.Status.Ancestors {
		if !ready(ancestor.Conditions) {
			return false
		}
	}
	return true
}

func (c *Cluster) verify(ctx context.Context) error {
	if _, err := c.kube(ctx, nil, "-n", "envoy-gateway-system", "rollout", "status", "deployment/envoy-gateway", "--timeout=120s"); err != nil {
		return fmt.Errorf("wait for Envoy Gateway: %w", err)
	}
	raw, err := c.kube(ctx, nil, "version", "-o", "json")
	if err != nil {
		return err
	}
	var version struct{ ServerVersion struct{ GitVersion string } }
	if json.Unmarshal(raw, &version) != nil || version.ServerVersion.GitVersion != strings.Replace(c.opts.K3SVersion, "-k3s", "+k3s", 1) {
		return errors.New("egtest: Kubernetes version differs from requested pin")
	}
	raw, err = c.kube(ctx, nil, "get", "nodes", "-o", "json")
	if err != nil {
		return err
	}
	var nodes struct{ Items []json.RawMessage }
	if json.Unmarshal(raw, &nodes) != nil || len(nodes.Items) != 1 {
		return errors.New("egtest: expected exactly one node")
	}
	raw, err = c.kube(ctx, nil, "get", "deployment/traefik", "-n", "kube-system", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) != 0 {
		return errors.New("egtest: bundled Traefik must be disabled")
	}
	for _, crd := range []string{"gatewayclasses.gateway.networking.k8s.io", "gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "envoyproxies.gateway.envoyproxy.io", "envoypatchpolicies.gateway.envoyproxy.io", "envoyextensionpolicies.gateway.envoyproxy.io"} {
		if _, err := c.kube(ctx, nil, "wait", "crd/"+crd, "--for=condition=Established", "--timeout=30s"); err != nil {
			return fmt.Errorf("wait for CRD %s: %w", crd, err)
		}
	}
	raw, err = c.kube(ctx, nil, "get", "deployment/envoy-gateway", "-n", "envoy-gateway-system", "-o", "json")
	if err != nil {
		return err
	}
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct{ Name, Image string }
				}
			}
		}
	}
	if json.Unmarshal(raw, &deployment) != nil {
		return errors.New("egtest: invalid controller deployment")
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "envoy-gateway" && strings.HasSuffix(strings.Split(container.Image, "@")[0], ":"+c.opts.EGVersion) {
			return nil
		}
	}
	return errors.New("egtest: controller image differs from requested version")
}
