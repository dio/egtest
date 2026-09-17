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
- Live installation and Linux execution were not exercised during the initial
  local extraction; subsequent CI evidence is recorded below.

The install-only live gate is `make integration` with explicit
`EGTEST_EG_VERSION` and `EGTEST_K3S_VERSION`. Application acceptance still belongs
to consumer tests. A permanent APIx consumer migration remains a separate step.
These checks were performed before the initial GitHub publication; publishing the
source does not add live installation or forwarding evidence.

## GitHub CI

[Run 35192093094](https://github.com/dio/egtest/actions/runs/35192093094),
revision `70da9427d490701c340935a1f5ff570559a39bfe`, passed all five jobs:

- Unit tests, race detector, vet and formatting on Ubuntu and macOS with Go 1.25
  and 1.27 (four jobs).
- Real single-server k3d installation on Linux/amd64: EG `v1.9.1`, k3s
  `v1.33.13-k3s2`, k3d `v5.8.3`, kubectl `v1.33.0`, Helm `v3.17.3`.
  The safe result artifact records `installation_verified: true`,
  `cleanup: verified`, and `passed: true`.

The initial CI attempt stopped before cluster creation because k3d's checksum
manifest names `_dist/k3d-linux-amd64`, while the download used a different path.
The corrected recipe preserves the manifest path and verifies the published
checksum. The successful run exercises that correction.

All workflow action references were checked against their official latest
releases and updated: checkout `v7.0.1`, setup-go `v7.0.0`, upload-artifact
`v7.0.1`. Go also resolved and downloaded the public module through normal module
tooling without a local workspace or replace directive.

This evidence qualifies cluster installation/removal on the CI Linux host.
It does not qualify APIx forwarding, dynamic-module loading, a shared-cluster
wrapper, or live installation on macOS.
