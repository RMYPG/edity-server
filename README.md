# edity-server

The server behind [Edity](https://github.com/RMYPG/edity), the inline content editor for RMYPG's static Astro Sites. One instance in the cluster serves every Site: it handles Client login through Zitadel, validates each Publish against the Site's Field definitions read from `main`, commits it as a GitHub App and reports progress until the Release is live.

The product spec, glossary and decisions live in the `edity` repository:

- Spec: [`.scratch/edity-v1/spec.md`](https://github.com/RMYPG/edity/blob/main/.scratch/edity-v1/spec.md)
- Glossary: [`CONTEXT.md`](https://github.com/RMYPG/edity/blob/main/CONTEXT.md)
- Why this is a separate Go repository: [ADR-0005](https://github.com/RMYPG/edity/blob/main/docs/adr/0005-save-service-in-go-in-its-own-repository.md)

## Development

Every development tool is declared in `mise.toml`. Install them with:

```bash
mise install
```

Run the checks CI runs, the Go, OpenAPI and workflow linters and the tests, with:

```bash
mise run ci
```

`mise run dev` starts the server on `localhost:8080` with the `dev` secrets of the `edity` Infisical project, which it reads as your own Infisical user: log in once with `infisical login`. It serves the Sites in [`sites.dev.json`](sites.dev.json), where `edity-sandbox` answers on `localhost`.

`mise run check:sandbox` publishes on `RMYPG/edity-sandbox` for real, with the same secrets: it changes a Field, waits for the Release, and puts it back, so it makes two commits on the sandbox's `main` and two Releases. It also checks that a Conflict and a broken Limit commit nothing. It takes as long as two runs of the sandbox's CI, and is not part of `mise run ci`.

`mise run check:progress` only reads: it asks GitHub, as the App, how far the two Publishes `check:sandbox` made on 2026-10-29 have got, which is `released` as v1.0.1 and v1.0.2, and checks that asking again costs nothing against the rate limit.

The tests replay what others answered, recorded once:

- `mise run record:progress` and `mise run record:issues` record GitHub's answers about `RMYPG/edity-sandbox` that `internal/progress` and `internal/issues` replay, with read-only calls made as whoever `gh` is logged in as.
- `mise run record:prettier <edity>` records what Prettier makes of `internal/content`'s cases, and `mise run generate:emoji <edity>` writes `internal/content/emoji.go`, the width tables Prettier fits lines with (see [Publish](#publish)). Both run the Prettier of the edity checkout at `<edity>`, after `mise run deps` there.
- `mise run record:core-violations <edity>` records what `@edity/core` finds in the contracts' page fixtures, which `internal/fields` compares with its own. The edity checkout has to be at the contracts tag `go.mod` pins, built with `mise run build`.

## Configuration

The server reads its environment at startup and refuses to start if a variable is missing or invalid.

| Variable | Value |
| --- | --- |
| `EDITY_ADDR` | Address to listen on, `:8080` by default |
| `EDITY_SITES_FILE` | Path of the Sites file |
| `EDITY_PUBLISHER_GITHUB_APP_ID` | ID of the `edity-publisher` GitHub App |
| `EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY` | Its private key, in PEM |

The Sites file lists every Site the server works for. A request whose hostname is not listed gets 404 on every `/_edity/` route.

```json
{
  "sites": [
    {
      "id": "edity-sandbox",
      "hostnames": ["sandbox.example.com"],
      "githubRepository": "RMYPG/edity-sandbox",
      "locale": "en",
      "zitadelRole": "site:edity-sandbox",
      "kubernetes": {"namespace": "edity-sandbox", "deployment": "edity-sandbox-sws"}
    }
  ]
}
```

The server refuses to start on a Sites file it can't read unambiguously: unknown or repeated keys (in any letter case), anything after the JSON document, or a Site field that is missing or malformed. `githubRepository` follows GitHub's naming rules, `locale` is a BCP 47 tag such as `es` or `es-ES`, `zitadelRole` has no spaces, and the namespace and Deployment are valid Kubernetes names.

`GET /healthz` answers Kubernetes probes on any hostname. Of the spec's API, `POST /_edity/api/publish` and `GET /_edity/api/publish/{sha}` are implemented, and the other routes answer 501 until they are. No Client can log in before the Zitadel login exists, so every Publish gets 401 for now. Logs are JSON lines on stdout, one per request.

## Publish

A Publish reads the page's content (`src/content/<page>.json`), its generated Field definitions (`src/content/<page>.schema.json`) and `package.json` from the Site repository at `main` HEAD, as the `edity-publisher` GitHub App. A Change whose stored value is not its `before`, or whose Field or Item is gone, is a Conflict, unless the stored value is its `after` already: that Change is already published, whatever its `before`, and has nothing to commit. The others merge, and the content they make must pass the Field definitions as a whole, Fields no Change touched included. The commit goes on top of that exact HEAD through GraphQL's `createCommitOnBranch`: it holds the content file and `package.json` with its patch bumped, and nothing else. If `main` moved in the meantime, the Publish reads it again and starts over, and gives up after three attempts.

When every Change is already published, as when a Client publishes again a Draft whose answer they missed, the Publish commits nothing and answers with the page's last Publish, the newest of the App's commits on `main` that changed the page's content, so that the Editor follows its progress. When the App made none, the Changes are answered as though none were already published.

The content file is written as Prettier formats JSON, keeping its keys' order and every value no Change touched, so that the Site's `prettier --check` passes (see [Content files and Prettier](#content-files-and-prettier)).

Each Site may publish ten times at once, and thirty times an hour after that.

## Content files and Prettier

A Site's CI runs `prettier --check`, so a content file the server writes has to be the one Prettier would write, or the Publish fails in CI (N3). The server does not run Prettier: `internal/content/format.go` ports what Prettier 3.9 does with a JSON file under its default options. It keeps a line break after each object's opening brace, puts a List on one line when it fits in 80 columns and else an Item per line, or as many numbers per line as fit, indents by 2 spaces and ends lines with `\n`. It measures a line as Prettier does, with Prettier's `getStringWidth` and its tables of emoji and East Asian wide characters, generated into `internal/content/emoji.go`.

So for `src/content/*.json` a Site keeps:

- **Prettier's defaults for JSON**: `printWidth` 80, `tabWidth` 2, `useTabs` false, `endOfLine` `lf`, `objectWrap` `preserve`, and the `json` parser Prettier picks for a `.json` file. No option in the Site's Prettier config, no `overrides` entry that matches those files, and no `.editorconfig` section that does (`indent_style`, `indent_size`, `tab_width`, `max_line_length`, `end_of_line`, which Prettier reads too) may set them otherwise.
- **Prettier 3.x, at the minor the tables come from**: 3.9 today, `prettierVersion` in `emoji.go`. Another minor can measure a line differently, as when it updates its emoji.

When the Sites move to another Prettier:

1. In an edity checkout whose `package.json` takes that Prettier, run `mise run deps`.
2. Run `mise run generate:emoji <edity>`. It fails if that Prettier's `getStringWidth` is no longer the function `internal/content/width.go` ports, which then has to be ported again.
3. Run `mise run record:prettier <edity>`, which records what that Prettier makes of the content tests' files, texts and numbers, and then `mise run test`, which checks `internal/content` against all of them.

## Publish progress

`GET /_edity/api/publish/{sha}` works out how far a Publish has got from the Site repository on GitHub, on each request, and stores nothing. Only a commit the App authored is a Publish's; any other sha gets 404.

- **validating** while the workflow runs that the commit's push started (`actions/runs?head_sha=…&event=push`) are under way, or before GitHub lists any. A run GitHub keeps `queued`, waiting for a runner, stays validating with no deadline: it starts once a runner is free, or the Developer cancels it, which fails the Publish.
- **releasing** once they all passed, until a Release contains the commit.
- **released** once the highest `vX.Y.Z` tag contains the commit (`compare/<sha>...<tag>`), the commit's own Release or a newer one. `release.yml` pushes the tag only once the image of that version exists. Tags only ever go on `main` with rising versions, so a tag older than the commit's `package.json` version is never compared.
- **failed** when a CI run of the push failed, or the commit's Release on Publish run did, or nothing came of it in time: no CI run 10 minutes after the commit, or no Release 30 minutes after CI passed while no Release on Publish run is under way, on a Site whose workflow names its runs (below). Any run under way may still release the commit, as a newer Release, so none may be. A newer Release that contains the commit still makes it released. It is answered only once an issue tells the Developer (see below).

A Release on Publish run is the commit's when it is titled `Release <sha>`, as the template names it, and was created after CI last started. A run's own `head_sha` is `main` HEAD when it started, which can be a newer Publish's commit, so it is not used, and neither are check runs, which mix every workflow's jobs on a sha. A cancelled run counts as one a newer run took the place of. Until a Site's workflow names its runs ([RMYPG/static-astro-site-template#15](https://github.com/RMYPG/static-astro-site-template/pull/15), [RMYPG/edity-sandbox#4](https://github.com/RMYPG/edity-sandbox/pull/4)), the commit is released by its tag alone, and no Release deadline applies: no run there tells whether the commit's own is still to come, so a slow Release would read failed, then released.

The App's rate limit is the installation's, 5,000 calls an hour shared by every Site. A request makes 4 calls, 5 while releasing. Each GET sends the ETag of GitHub's last answer to it, and GitHub does not count a call it answers 304, so a request that finds nothing new costs nothing, and requests for one commit within 5 seconds share one look. The tests count it: an hour of polling one Publish every 5 seconds makes about 2,900 calls, of which GitHub counts 17.

## Issues on the Site repository

The App opens GitHub issues on the Site repository to report to the Developer what they need to know, and stores nothing else.

- **A Publish that failed** (V6). The Editor tells the Client that their changes are saved, are not live yet, and that the Developer has been notified (P5), so the server answers `failed` only once an issue says what failed, for which Publish (its commit, version, page, the Fields and Lists it changed and the Client), with a link to the run. Until it is written, the progress answers 503, or 500 when GitHub refuses it, such as when the repository's Issues are turned off.
- **Commits that are not content** (V7). A Publish ships everything on `main` since the last Release. Once a Publish is answered, the server compares the highest `vX.Y.Z` tag with its commit, and reports each commit the App did not make that changes files outside `src/content/`, or more of `package.json` than its version. It never delays or fails the Publish: a failure to report them is logged, and the next Publish before the next Release reports them.

Each kind has one open issue at a time, which the App opened and which a hidden marker in its body names. Every failure, and every Publish that ships more such commits, is a comment on it. The body lists, in another hidden marker, what the issue told: a failed run by its ID and attempt, so a run run again that fails again is told again, or a missed deadline by the commit. A failure polled again, or by a server that restarted, is not told twice. One call finds the issues, the App's 30 updated last, closed ones too, and it costs nothing while they stay the same. Once the Developer closes the issue, what it told is not told again, and the next news opens a new issue.

The issue is opened when something asks for the progress of the failed Publish: the Editor, while the Client watches it, and a sweep, since a Client who closes the tab stops asking. Every 5 minutes, from when it starts, the server sweeps each Site in turn: it compares the highest `vX.Y.Z` tag with `main` (or lists `main`'s commits while there is no Release), and asks the progress of each of the App's commits since, its newest 10, as a request would, so that the same checks keep a failure from being told twice. A sweep stores nothing, logs one line per Site, and stops when the server does.

The sweeps cost the rate limit little next to the Editor's polling. The tests count them (`TestSweepingStaysWithinTheRateLimit`): a Site with nothing since its last Release makes 2 calls a sweep, 25 an hour, of which GitHub counts 5, the first answers and the same again once the token is replaced. A Site whose Publish stays under way all hour makes 7 calls a sweep, 85 an hour, of which GitHub counts 13. Twenty Sites, each with a Publish under way, would cost some 260 of the 5,000 calls an hour every Site shares.

## API

[`api/openapi.yaml`](api/openapi.yaml) describes the server's HTTP API in OpenAPI 3.1: every route the Editor and the browser call on a Site's origin, every status each one answers, and every body. It is the contract between the server and the Editor.

- The `edity` repository generates the Editor's API client from it, fetched at a pinned commit of this repository's `main`: `https://raw.githubusercontent.com/RMYPG/edity-server/<commit>/api/openapi.yaml`. It is one file with no references to others, so that fetch is the whole contract, and a commit's file never changes.
- The handlers' tests check their requests and responses against it. Wrap the server in `apitest.Handler` (`internal/apitest`) in a test, and every response it gives must be one the file describes, every request the file does not allow must get a 4xx, and every route the file does not have must get 404 or 405.
- It is licensed under [MPL-2.0](api/LICENSE), not AGPL-3.0 like the rest of the server, because `@edity/astro` ships the API client generated from it.
- `mise run lint` lints it with [vacuum](https://quobix.com/vacuum/), with the rules in [`api/vacuum-ruleset.yaml`](api/vacuum-ruleset.yaml).

Sites built with an older Editor keep calling the server, so a change to the file keeps the calls of earlier Editors working.

## Image

CI builds a static binary on `gcr.io/distroless/static-debian13:nonroot` and starts it before anything is pushed. Pushes to `main` push it to `ghcr.io/rmypg/edity-server`, tagged `sha-<commit>` and `development`. When pushes to `main` arrive faster than CI runs, the commits in between are skipped, and `development` always ends on the latest one. It listens on port 8080 and runs as a non-root user.

## License

[AGPL-3.0](LICENSE), except [`api/openapi.yaml`](api/openapi.yaml), which is [MPL-2.0](api/LICENSE) like the edity packages: it is the contract the Editor's API client is generated from, and `@edity/astro` ships that client. The reasons are in [ADR-0006](https://github.com/RMYPG/edity/blob/main/docs/adr/0006-mpl-for-the-packages-agpl-for-the-server.md).
