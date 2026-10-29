#!/usr/bin/env bash
# Records the GitHub responses progress_test.go replays, from RMYPG/edity-sandbox:
# the two Publishes of 2026-10-29 and the Developer commit before them. Every
# call is a read, made as whoever gh is logged in as. Each file keeps the
# status line, the headers the server reads and the body, as GitHub sent them.
# Run it with `mise run record:progress`.
set -euo pipefail

repo=repos/RMYPG/edity-sandbox
first=4bc931076ac8583bd8905a4c508c987877afc09d  # v1.0.1
second=f50d2c45e28706d25da874225f0f64770cd1ac12 # v1.0.2
developer=31077c358f1b793aefc1c495c938055d99d3c227

# shellcheck source-path=SCRIPTDIR source=../../githubtest/record.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../githubtest/record.sh"

for sha in "$first" "$second" "$developer"; do
	record "commit-${sha:0:7}.http" "$repo/git/commits/$sha"
	record "package-${sha:0:7}.http" "$repo/contents/package.json?ref=$sha" application/vnd.github.raw+json
done
for sha in "$first" "$second"; do
	record "runs-${sha:0:7}.http" "$repo/actions/runs?head_sha=$sha&event=push&exclude_pull_requests=true"
done
record tags.http "$repo/git/matching-refs/tags/v"
record "compare-${first:0:7}-v1.0.1.http" "$repo/compare/$first...v1.0.1?per_page=1"
record "compare-${first:0:7}-v1.0.2.http" "$repo/compare/$first...v1.0.2?per_page=1"
record "compare-${second:0:7}-v1.0.2.http" "$repo/compare/$second...v1.0.2?per_page=1"
record release-runs.http "$repo/actions/workflows/release-on-publish.yml/runs?event=workflow_run&per_page=20"
