# Working with egtest

Read [README.md](README.md) for the public API and
[VERIFICATION.md](VERIFICATION.md) for actual evidence and its limits.

## Purpose and boundaries

`github.com/dio/egtest` is a Go library for disposable k3d/Envoy Gateway fixtures.
It currently has no CLI or cross-package shared-cluster wrapper. Prefer a small
API that makes the consuming developer's test easy to read and run.

egtest owns cluster creation, a private kubeconfig, EG installation, scoped
commands, port forwards and removal. Consumers own image builds, application
manifests, request behavior, backend receipts, credentials and application-specific
assertions. Do not add APIx, Transit or another consumer's policy to this library.

## Using the library in a consumer

1. Pin an actual published revision in the consumer's `go.mod`. Use a temporary
   Go workspace for unpublished development; do not commit machine-specific
   `replace` directives. Docker builds must also be able to resolve the revision.
2. Use `egtest.New(t, Options)` for a parent test and its subtests. It registers
   cleanup with `t.Cleanup`. Never defer `Close` on a parent with parallel subtests.
   For `TestMain`, use `Open(ctx, Options)` and close before calling `os.Exit`.
3. Pass explicit `EGVersion` and `K3SVersion` values. Keep YAML in a readable
   fixture file and use `go:embed` to supply `HelmValues` as `[]byte`, following the
   README and `integration_test.go`. Generated values are also supported. Never
   embed live credentials or secrets in test binaries.
4. Gate expensive cluster tests explicitly in the consumer. Keep fast tests
   independent of Docker, and report skipped live tests as not run.
5. Build images using the consumer's existing tooling, then call `ImportImages`.
   Apply consumer manifests with `Apply`; use `Kubectl` for remaining scoped
   operations. Do not fall back to the user's default kubeconfig or context.
6. Use `WaitDeployment`, `WaitProgrammed`, and `PortForward` as appropriate.
   A forward's `URL` is available after startup; close the handle when finished.
   Cluster cleanup also stops all owned forwards.
7. Assert application behavior in the consumer. Controller status and installation
   success do not prove active Envoy configuration, module loading or forwarding.
   Authorization tests need responses, terminal observations and backend
   receipt/non-receipt evidence appropriate to the application.
8. Stop application workers before closing the cluster. Handle cleanup errors.
   For reports, read `Info()` after `Close`; failed setup exposes cleanup evidence
   through `*SetupError`. A retained cluster is not a verified removal.

Do not duplicate the generic cluster lifecycle in each consumer. Extend this
library only when a concrete consumer job needs a reusable operation.

## Maintaining the package

- Inspect git status before editing and preserve unrelated work. Commit or push
  only within the user's authorization.
- Keep the error-returning core independent of a test framework. The `New`
  convenience function uses `testing.TB`; do not require testify or a CLI.
- Preserve private defaults: unique owned names, loopback API/forward ports,
  explicit kubeconfig scoping, no cluster adoption, no global environment changes.
- Keep setup, operations, child processes and cleanup bounded and cancellable.
  Setup cancellation must clean up partial resources with a fresh deadline.
  Preserve idempotent, concurrency-safe close and rejection after close.
- Never log command arguments, stdin, credentials, request bodies, query strings,
  raw Kubernetes Secrets, or raw Envoy configuration. Use structured safe
  diagnostics. `Kubectl` output belongs to the caller and may contain secrets.
- Preserve exact ownership during removal. Do not delete unrelated clusters,
  images, volumes or processes. `Keep` stops forwards but retains resources with
  explicit inventory and removal instructions.
- Keep manifests readable. Prefer embedded YAML fixtures over escaped multiline
  strings in Go examples/tests. Use existing abstractions and avoid adding options
  without a concrete test job.
- Retain source attribution in NOTICE and LICENSE when extracting code/patterns.

## Verification and CI

Run `make check` for Go changes: vet and race-enabled tests. Add regression tests
for lifecycle, isolation, concurrency, cancellation or error-handling changes.
Fake executable tests must never reach real Docker/Kubernetes tools.

For installation changes, run the live gate on a suitable host or in CI:

```sh
EGTEST_EG_VERSION=v1.9.1 EGTEST_K3S_VERSION=v1.33.13-k3s2 make integration
```

This creates and removes real resources. The CI install job uploads an allowlisted
JSON result. Inspect the job and cleanup evidence before claiming success.
Record newly verified platforms/version pairs and meaningful failures/corrections
in VERIFICATION.md; do not turn skipped/fake tests into live qualification.

When changing GitHub Actions, check every `uses:` reference against the official
release, pin the selected version, and verify the workflow. Keep the unit/race
matrix and the separate real installation/cleanup job intact.
