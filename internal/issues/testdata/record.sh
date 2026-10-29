#!/usr/bin/env bash
# Records the GitHub responses issues_test.go replays, from RMYPG/edity-sandbox:
# its issue list, with none of the App's yet; pull request #4 as the issues
# API lists it, the shape every issue has; the tags; and the commits from
# 0345f67, before the Developer's last two, to the first Publish of
# 2026-10-29, and from v1.0.0 to it. Every call is a read, made as whoever gh
# is logged in as. Each file keeps the status line, the headers the server
# reads and the body, as GitHub sent them. Run it with `mise run record:issues`.
set -euo pipefail

repo=repos/RMYPG/edity-sandbox
first=4bc931076ac8583bd8905a4c508c987877afc09d  # v1.0.1
before=0345f674a02fb499b9e8e1a9cfc4b788db1549e6 # main before the Developer's last commits
developer=7b2ba675cfb202e9860bae12a70ace8bf6ba2d44
merge=31077c358f1b793aefc1c495c938055d99d3c227 # v1.0.0

# shellcheck source-path=SCRIPTDIR source=../../githubtest/record.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../githubtest/record.sh"

record issues.http "$repo/issues?state=all&creator=edity-publisher%5Bbot%5D&sort=updated&direction=desc&per_page=30"
record pull-4.http "$repo/issues/4"
record tags.http "$repo/git/matching-refs/tags/v"
record "compare-${before:0:7}-${first:0:7}.http" "$repo/compare/$before...$first"
record "compare-v1.0.0-${first:0:7}.http" "$repo/compare/v1.0.0...$first"
for sha in "$developer" "$merge"; do
	record "commit-${sha:0:7}.http" "$repo/commits/$sha"
done
