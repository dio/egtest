---
name: use-egtest
description: Integrate github.com/dio/egtest into a Go project's Envoy Gateway tests, or replace duplicated k3d/EG lifecycle helpers with the library. Use for consumer setup, embedded Helm fixtures, cleanup, and CI live checks; not for general Kubernetes deployment.
---

# Use egtest in a Go project

Help the developer write a readable test using an owned, disposable EG cluster.
Keep application behavior in the consuming project. egtest is a **library**:
there is no `egtest` CLI, daemon, or cross-package shared-cluster wrapper.

## Establish the consumer boundary

Read the project's contribution instructions, Go version, current test setup,
Make targets and CI before changing them. Identify which existing code owns:

- cluster/EG setup, kubeconfig scoping, port forwards and cleanup;
- application manifests, image builds, requests and assertions.

Replace only the generic lifecycle work. Keep the consumer's application checks,
version choices, image build process and explicit test gates. Do not create a new
Go module just for the tests unless isolation actually requires it.

Check the API of the revision being consumed with `go doc` or its downloaded
source. The [README](https://github.com/dio/egtest/blob/main/README.md) documents the
current API and coverage limits;
[GitHub Actions](https://github.com/dio/egtest/actions/workflows/test.yaml) provides
current results and per-run artifacts. Neither overrides the API of an older
pinned revision.

## Add the dependency and readable fixtures

Use a real published revision. For first adoption, `go get github.com/dio/egtest@latest` records a concrete version in `go.mod`; preserve a
user-selected version instead when supplied. Inspect the dependency diff. For
unpublished library work, use a temporary workspace rather than committing an
absolute-path `replace`. Docker build contexts must resolve the same dependency.

Store Helm values in a YAML fixture and embed it in the consuming test:

```go
import _ "embed"

//go:embed testdata/helm-values.yaml
var helmValues []byte
```

Pass `HelmValues: helmValues`. Keep the public `[]byte` contract: callers may also
generate values. Do not turn YAML into an escaped multiline Go string, require
an absolute fixture path, or embed live credentials. Application manifests can
use the same embedding pattern where appropriate.

Use explicit `EGVersion` and `K3SVersion` values from the consumer's qualification
policy. A successful library installation pair is a starting point, not proof
that the consumer's custom Envoy/module image is compatible.

## Wire ownership

For a parent test, prefer:

```go
cluster := egtest.New(t, egtest.Options{
    EGVersion:  egVersion,
    K3SVersion: k3sVersion,
    HelmValues: helmValues,
})
```

`New` registers `t.Cleanup` and survives parallel subtests. Never defer `Close`
on a parent whose parallel subtests still need the cluster. For `TestMain` or a
suite adapter that handles errors, use `Open(ctx, Options)`, close explicitly,
and call `os.Exit` only after cleanup. The context supplied to `Open` bounds
setup, not the lifetime after successful return.

Use `ImportImages` for existing local tags, `Apply` for manifest bytes, and
`Kubectl` for other operations scoped to the private kubeconfig. Avoid parallel
lifecycle code, global context changes, fixed-name cluster adoption, and shell
commands that fall back to the user's current Kubernetes context.

Use `WaitDeployment`/`WaitProgrammed` where applicable. Start a forward with
`PortForward(ctx, namespace, target, remotePort)` and use its `URL`; close the
handle when finished. `Cluster.Close` also stops owned forwards. Stop application
workers before cluster cleanup. Image creation/deletion remains caller-owned.

Use `Keep` only when the user wants retained resources for debugging; report the
exact owned cluster, private kubeconfig location and cleanup instructions.
Ordinary test failures should still attempt cleanup.

## Verify the integration

Keep expensive tests explicitly gated according to the consumer's conventions.
A skipped live test is not live qualification evidence. Run focused compilation and
existing relevant tests first. Run the live lane on an authorized suitable host
or the project's CI; do not silently switch to a shared cluster or provision a
cloud build host when local prerequisites are missing.

Distinguish three levels of evidence:

- Fake-command tests verify lifecycle/error handling without real Kubernetes.
- The library live lane verifies installation, image import, readiness, forwarding,
  retention and removal using a generic fixture; see README for platform coverage.
- Consumer tests verify active Envoy topology, responses and backend behavior.

Controller `Programmed` status alone does not prove Envoy accepted an xDS update
or protected a request. Preserve receipt/non-receipt and terminal observations
when the consumer tests authorization or forwarding.

Capture `Info()` after `Close()` for cleanup reports. On setup failure, unwrap
`*egtest.SetupError` for its `Info` even though there is no cluster handle. Handle
ordinary prerequisite errors separately. Cleanup failure and deliberate retention
must remain visible. Never log Secrets, raw Envoy config, credentials or command
stdin; raw `Kubectl` stdout can contain such data.

If adding CI, keep unit/race checks separate from a real cluster job, pin tool
versions and current official action releases, and upload only safe result data.
Report which gates actually ran, their outcomes, remaining limitations and any
retained resources. Do not claim a platform or consumer boundary from compilation
alone. Do not publish the consumer or modify unrelated automation as a side effect
of using this skill.
