#!/usr/bin/env bash

set -euo pipefail

readonly DEFAULT_REPO_URL="https://github.com/justinswe/jarvis.git"

sanitize_repository_url() {
  sed -E \
    -e 's#(https?://)[^/@]+:[^/@]+@#\1#' \
    -e 's#(https?://)[^/@]+@#\1#'
}

print_defaults() {
  printf 'REPO_URL %s\n' "${DEFAULT_REPO_URL}"
  printf 'COMMIT_SHA dev\n'
  printf 'GIT_BRANCH unknown\n'
  printf 'GIT_TREE_STATUS Unknown\n'
  printf 'STABLE_COMMIT_SHA dev\n'
}

if ! git rev-parse --git-dir >/dev/null 2>&1; then
  print_defaults
  exit 0
fi

repo_url="$(git config --get remote.origin.url || true)"
if [[ -z "${repo_url}" ]]; then
  repo_url="${DEFAULT_REPO_URL}"
else
  repo_url="$(printf '%s' "${repo_url}" | sanitize_repository_url)"
fi

commit_sha="$(git rev-parse HEAD)"
git_branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "${git_branch}" == "HEAD" ]]; then
  git_branch="${GITHUB_HEAD_REF:-${GIT_BRANCH:-HEAD}}"
fi

if git diff-index --quiet HEAD --; then
  git_tree_status="Clean"
else
  git_tree_status="Modified"
fi

printf 'REPO_URL %s\n' "${repo_url}"
printf 'COMMIT_SHA %s\n' "${commit_sha}"
printf 'GIT_BRANCH %s\n' "${git_branch}"
printf 'GIT_TREE_STATUS %s\n' "${git_tree_status}"
printf 'STABLE_COMMIT_SHA %s\n' "${commit_sha}"
