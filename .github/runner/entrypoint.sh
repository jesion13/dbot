#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_PAT:?set GITHUB_PAT (classic PAT with 'repo' scope)}"
: "${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY as owner/repo, e.g. fr-str/dbot}"

RUNNER_LABELS="${RUNNER_LABELS:-self-hosted,dbot}"
RUNNER_NAME="${RUNNER_NAME:-dbot-runner-$(hostname)}"

# Named volumes mount root-owned; fix up so the unprivileged runner user can
# write its Go module/build cache into them.
sudo chown -R runner:runner /go/pkg/mod /home/runner/.cache

cd /home/runner/actions-runner

# restart: always reuses this container, so a stale .runner from the previous
# boot makes config.sh refuse to start. Wipe it; --replace handles GitHub's side.
rm -f .runner .credentials .credentials_rsa .credentials_ecdsa .svc

REG_TOKEN=$(curl -fsSL -X POST \
    -H "Authorization: Bearer ${GITHUB_PAT}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${GITHUB_REPOSITORY}/actions/runners/registration-token" \
    | jq -r .token)

if [ -z "${REG_TOKEN}" ] || [ "${REG_TOKEN}" = "null" ]; then
    echo "failed to fetch a runner registration token - check GITHUB_PAT scope and GITHUB_REPOSITORY" >&2
    exit 1
fi

./config.sh \
    --url "https://github.com/${GITHUB_REPOSITORY}" \
    --token "${REG_TOKEN}" \
    --name "${RUNNER_NAME}" \
    --labels "${RUNNER_LABELS}" \
    --work "_work" \
    --ephemeral \
    --unattended \
    --replace

cleanup() {
    echo "removing runner..."
    UNREG_TOKEN=$(curl -fsSL -X POST \
        -H "Authorization: Bearer ${GITHUB_PAT}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${GITHUB_REPOSITORY}/actions/runners/registration-token" \
        | jq -r .token)
    ./config.sh remove --token "${UNREG_TOKEN}" || true
}
trap cleanup SIGINT SIGTERM EXIT

./run.sh
