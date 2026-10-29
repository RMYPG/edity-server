# shellcheck shell=bash
# Sourced by each package's testdata/record.sh, which records the GitHub
# answers its tests replay (Recorded reads them back).

# record <file> <path> [<accept>] writes GitHub's answer to a GET of path,
# made as whoever gh is logged in as, to file: the status line, the headers
# the server reads and the body, as GitHub sent them.
record() {
	gh api --include --method GET \
		--header "X-GitHub-Api-Version: 2026-03-10" \
		--header "Accept: ${3:-application/vnd.github+json}" "$2" |
		tr -d '\r' |
		awk 'NR == 1 { print; next }
			body { print; next }
			/^$/ { print; body = 1; next }
			tolower($0) ~ /^(content-type|etag|last-modified|link|x-github-api-version-selected):/ { print }' >"$1"
}
