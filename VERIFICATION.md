# Verification record

2026-09-17, local macOS/arm64, Go 1.27.1:

- `make check`: vet and race-enabled unit tests passed. These use private fake
  executables and do not create a Docker container or Kubernetes cluster.
- APIx consumer check: a temporary Go workspace contained APIx and this module.
  A Go source overlay replaced APIx's `gatewaySuite` installation/cleanup with a
  thin `egtest.Open`/`Close` adapter and delegated `kube` to `Cluster.Kubectl`.
  `go test -race -count=1 -overlay <overlay.json> ./e2e/eg/...` passed, including
  APIx's child-process Helm-failure cleanup test. APIx source, `go.mod`, and
  `go.sum` were unchanged. The full scenario compiled but its live gate was skipped.
- Real EG installation, application forwarding, and Linux execution of this
  package are **not yet qualified**. The included CI workflow has not been run.

The install-only live gate is `make integration` with explicit
`EGTEST_EG_VERSION` and `EGTEST_K3S_VERSION`. Application acceptance still belongs
to consumer tests. A permanent APIx consumer migration remains a separate step.
These checks were performed before the initial GitHub publication; publishing the
source does not add live installation or forwarding evidence.
