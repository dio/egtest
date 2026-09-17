#!/usr/bin/env bash
set -euo pipefail

# This script targets GitHub's Linux/amd64 hosted runner. Downloads are verified
# against the checksums published alongside each exact upstream release.
test "$(uname -s)" = Linux
test "$(uname -m)" = x86_64
task_tools_dir=$(mktemp -d)
trap 'rm -rf "$task_tools_dir"' EXIT
cd "$task_tools_dir"

k3d_version=v5.8.3
mkdir _dist
curl --fail --location --retry 3 -o _dist/k3d-linux-amd64 "https://github.com/k3d-io/k3d/releases/download/$k3d_version/k3d-linux-amd64"
curl --fail --location --retry 3 -o checksums.txt "https://github.com/k3d-io/k3d/releases/download/$k3d_version/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
sudo install -m 0755 _dist/k3d-linux-amd64 /usr/local/bin/k3d

kubectl_version=v1.33.0
curl --fail --location --retry 3 -o kubectl "https://dl.k8s.io/release/$kubectl_version/bin/linux/amd64/kubectl"
curl --fail --location --retry 3 -o kubectl.sha256 "https://dl.k8s.io/release/$kubectl_version/bin/linux/amd64/kubectl.sha256"
printf '%s  kubectl\n' "$(cat kubectl.sha256)" | sha256sum --check
sudo install -m 0755 kubectl /usr/local/bin/kubectl

helm_version=v3.17.3
helm_archive="helm-$helm_version-linux-amd64.tar.gz"
curl --fail --location --retry 3 -O "https://get.helm.sh/$helm_archive"
curl --fail --location --retry 3 -O "https://get.helm.sh/$helm_archive.sha256sum"
sha256sum --check "$helm_archive.sha256sum"
tar -xzf "$helm_archive"
sudo install -m 0755 linux-amd64/helm /usr/local/bin/helm

docker info --format '{{.OSType}}/{{.Architecture}}'
k3d version
kubectl version --client
helm version --short
