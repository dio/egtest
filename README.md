# egtest

[![test](https://github.com/dio/egtest/actions/workflows/test.yaml/badge.svg)](https://github.com/dio/egtest/actions/workflows/test.yaml)

Disposable **k3d + Envoy Gateway** fixtures for Go tests. Each fixture owns one
single-server cluster, a private kubeconfig, and its port-forward processes.
Setup failure and cancellation trigger cleanup. No current-context changes,
cluster adoption, registry publishing, or testify dependency.

This module is experimental. CI verifies unit/race tests on Linux and macOS and
real EG installation/cleanup on Linux. Application forwarding remains a consumer
responsibility; see [the verification record](VERIFICATION.md).

## Install

```sh
go get github.com/dio/egtest@latest
```

The initial publication uses the revision on `main`; Go records its pseudo-version
in your `go.mod`. The API is experimental; installation evidence does not establish
application or extension compatibility.

## Use

Requires Go 1.25+ and the `docker`, `k3d`, `kubectl`, and `helm` executables.
Docker must be running. Consumers supply explicit EG and k3s version pins and
qualify that pair on their test host. `K3SVersion` accepts either the image-tag
spelling (`v1.33.13-k3s2`) or server GitVersion spelling (`v1.33.13+k3s2`);
`Info()` reports the image-tag spelling.

```go
//go:embed testdata/helm-values.yaml
var helmValues []byte

func TestGateway(t *testing.T) {
    if os.Getenv("RUN_EG") != "1" {
        t.Skip("set RUN_EG=1 to create a private cluster")
    }
    cluster := egtest.New(t, egtest.Options{
        EGVersion:  "v1.9.1",
        K3SVersion: "v1.33.13-k3s2",
        HelmValues: helmValues,
    })
    // Build images in your project, then import their existing local tags.
    if err := cluster.ImportImages(t.Context(), "my-proxy:test"); err != nil {
        t.Fatal(err)
    }
    if err := cluster.Apply(t.Context(), gatewayManifest); err != nil {
        t.Fatal(err)
    }
    if err := cluster.WaitProgrammed(t.Context(), "default", "gateway", "example"); err != nil {
        t.Fatal(err)
    }
    // Your test owns the application's requests, assertions and evidence.
}
```

Import `_ "embed"`, `github.com/dio/egtest`, `os`, and `testing`; supply your
project's `gatewayManifest`. Store Helm configuration in
[`testdata/helm-values.yaml`](testdata/helm-values.yaml):

```yaml
config:
  envoyGateway:
    extensionApis:
      enableEnvoyPatchPolicy: true
```

Embedding keeps the fixture available in compiled test binaries without relying
on the working directory. `HelmValues` accepts `[]byte`, so generated values also
work; the library does not impose a filesystem or embed your application's secrets.

The version pins above passed the Linux CI installation gate;
application behavior still needs its own tests. `New` registers cleanup on the
parent test, so the cluster remains alive
through parallel subtests. Do not use `defer cluster.Close()` on a parent with
parallel subtests.

For `TestMain` or non-testing callers, use `Open(ctx, Options)` and call `Close`
before `os.Exit`. `Open` returns errors instead of terminating a test. Setup
contexts govern setup only: cancellation after a successful return does not
destroy a running cluster. `Close` cancels in-flight operations, stops forwards,
deletes the owned cluster, verifies its Docker resources are absent, and removes
the private kubeconfig. Close is idempotent and safe to call concurrently.

## API and ownership

| API | Responsibility |
| --- | --- |
| `New(t, Options)` | Create a fixture with automatic test cleanup |
| `Open(ctx, Options)` | Create a fixture with error-returning setup |
| `cluster.Apply(ctx, manifest)` | Apply JSON/YAML through private kubeconfig |
| `cluster.Kubectl(ctx, stdin, args...)` | Scoped command; raw output belongs to the caller |
| `cluster.ImportImages(ctx, tags...)` | Import local images into the owned cluster |
| `cluster.WaitDeployment(ctx, namespace, name)` | Wait for a deployment rollout |
| `cluster.WaitProgrammed(ctx, namespace, kind, name)` | Check current-generation Gateway or EnvoyPatchPolicy conditions |
| `cluster.PortForward(ctx, namespace, target, port)` | Allocate a loopback port and return an owned `Forward` with `URL` and `Close` |
| `cluster.Info()` | Version pins, installation gate and cleanup result |
| `cluster.Close()` | Stop owned processes and remove owned resources |

Options include `Prefix` (default `egtest`, always with a random suffix),
`Timeout` (ten minutes per setup/operation), `CleanupTimeout` (two minutes for
resource removal), `Keep`, and an optional `*slog.Logger`. Earlier caller
deadlines take precedence. Child-process pipe shutdown has an additional bounded
drain (up to two seconds). `Close` first cancels active operations and stops
forwards before starting/finishing resource removal. Application workers should
be stopped by the caller before closing the cluster.

The package verifies a single node, the requested k3s version, disabled Traefik,
required Gateway/EG CRDs, controller rollout, and controller image version.
Controller `Programmed` status does **not** prove Envoy accepted or enforced
configuration. Inspect active xDS and test responses/backend receipts in the
consumer. APIx-specific Pax, quota, credentials and module topology stay in APIx.

There is no run-wide shared cluster or CLI wrapper yet. Namespaces do not isolate
cluster-wide CRDs, GatewayClasses or controller settings; a private cluster per
suite makes ownership explicit. Testify suites can consume `Open` through a thin
adapter without making testify a library dependency.

For server-side apply, use the scoped command API:

```go
_, err := cluster.Kubectl(ctx, manifest, "apply", "--server-side", "-f", "-")
```

Cluster-selection and TLS-verification overrides are rejected; application flags
such as `--server-side` and caller-selected authentication flags remain available.

## Failures, diagnostics and removal

`Open` cleans up partial setup with a fresh context. `*SetupError` exposes an
`Info` value after cleanup, so the consumer can write a failure report even when
no cluster handle was returned. Prerequisite/option failures before resource
allocation return an ordinary error. `*CommandError` preserves tool identity and
exit status/cancellation while excluding arguments and subprocess output, even
from the wrapped exit error. Raw stdout from `Kubectl` may contain credentials;
the caller must handle it privately.

`Keep: true` stops port forwards but retains the cluster and private kubeconfig,
including after setup failure. Logs/error details identify the resources. Remove
the exact cluster with `k3d cluster delete <Info.Name>`, then remove the containing
private kubeconfig directory. Cleanup failures also retain private files and
report their path. Never substitute another cluster name. `New` reports retained
resources through test logs; `Open` uses the supplied logger and `Info`/`SetupError`.

Host image builds, image deletion, downloaded tools and cloud build VMs belong
to the caller. k3d-owned containers, network and image volume belong to egtest.
Process termination by SIGKILL cannot execute Go cleanup; use recorded ownership
information for manual removal. This package has no persistent daemon or janitor.

## Agent usage skill

The [use-egtest skill](skills/use-egtest/SKILL.md) guides an agent through adopting
the library in another Go project: dependency pinning, embedded YAML, lifecycle
ownership, application assertions and CI evidence.

From this checkout, install it for Codex:

```sh
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
cp -R skills/use-egtest "${CODEX_HOME:-$HOME/.codex}/skills/"
```

Reload skills or start a new session, then invoke:

```text
Use $use-egtest to integrate egtest into this project's Envoy Gateway tests.
```

## Local development

Agent-assisted contributors and consumers can follow [AGENTS.md](AGENTS.md) for
integration, lifecycle and verification guidance.

Use an uncommitted temporary Go workspace containing the consumer and this
module to test unpublished changes. Keep absolute-path `replace` directives out
of the consumer's committed `go.mod`. Consumers and Docker builds can otherwise
use the published revision through normal Go module tooling.

## Verification

CI runs vet, race tests and formatting checks on Linux and macOS with Go 1.25 and
1.27. A separate Linux/amd64 job installs pinned k3d, kubectl and Helm, creates
the real cluster, verifies installation and removal, and uploads a safe JSON
result. All action references use the official releases checked at publication:
checkout `v7.0.1`, setup-go `v7.0.0`, and upload-artifact `v7.0.1`.

```sh
make check
# Expensive; creates one real cluster and requires working Docker/k3d/Helm.
EGTEST_EG_VERSION=v1.9.1 EGTEST_K3S_VERSION=v1.33.13-k3s2 make integration
```

Unit tests use private fake executables, never real cluster commands. They cover
partial creation/Helm failure, private command scoping, image import, port-forward
lifetime and early exit, cancellation, concurrent close, keep-for-debug, cleanup
failure, generation checks and ancestor isolation. The gated integration test
qualifies installation and cleanup only. Neither skipped tests nor fake processes
qualify a live version pair, application forwarding, or a platform.

## Origin

EG mechanics are adapted from `dio/transit` at
`0411a9e53727bb9700f88493e97250ea8176c09d`,
`integrations/internal/egtest/egtest.go`, and the APIx EG harness derived from it.
The private kubeconfig, unique ownership and cleanup checks from APIx are retained.
The public `Open`/`New`/`Close` ownership pattern is inspired by `dio/pgtest`;
no pgtest implementation was copied. Licensed under Apache-2.0; see LICENSE.
