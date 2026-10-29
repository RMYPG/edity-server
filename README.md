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

`GET /healthz` answers Kubernetes probes on any hostname. Of the spec's API, `POST /_edity/api/publish` is implemented, and the other routes answer 501 until they are. No Client can log in before the Zitadel login exists, so every Publish gets 401 for now. Logs are JSON lines on stdout, one per request.

## Publish

A Publish reads the page's content (`src/content/<page>.json`), its generated Field definitions (`src/content/<page>.schema.json`) and `package.json` from the Site repository at `main` HEAD, as the `edity-publisher` GitHub App. A Change whose stored value is not its `before`, or whose Field or Item is gone, is a Conflict. The others merge, and the content they make must pass the Field definitions as a whole, Fields no Change touched included. The commit goes on top of that exact HEAD through GraphQL's `createCommitOnBranch`: it holds the content file and `package.json` with its patch bumped, and nothing else. If `main` moved in the meantime, the Publish reads it again and starts over, and gives up after three attempts.

The content file is written as Prettier formats JSON, keeping its keys' order and every value no Change touched, so that the Site's `prettier --check` passes. `internal/content` ports Prettier's line fitting, with the width tables of the Prettier the Sites use: `internal/content/emoji.mjs` writes `emoji.go` from its bundle, and `internal/content/testdata/prettier.mjs` records what Prettier itself makes of the test cases. Run both again when the Sites move to another Prettier.

Each Site may publish ten times at once, and thirty times an hour after that.

## API

[`api/openapi.yaml`](api/openapi.yaml) describes the server's HTTP API in OpenAPI 3.1: every route the Editor and the browser call on a Site's origin, every status each one answers, and every body. It is the contract between the server and the Editor.

- The `edity` repository generates the Editor's client from it, fetched at a pinned commit of this repository's `main`: `https://raw.githubusercontent.com/RMYPG/edity-server/<commit>/api/openapi.yaml`. It is one file with no references to others, so that fetch is the whole contract, and a commit's file never changes.
- The handlers' tests check their requests and responses against it. Wrap the server in `apitest.Handler` (`internal/apitest`) in a test, and every response it gives must be one the file describes, every request the file does not allow must get a 4xx, and every route the file does not have must get 404 or 405.
- It is licensed under [MPL-2.0](api/LICENSE), not AGPL-3.0 like the rest of the server, because `@edity/astro` ships the client generated from it.
- `mise run lint` lints it with [vacuum](https://quobix.com/vacuum/), with the rules in [`api/vacuum-ruleset.yaml`](api/vacuum-ruleset.yaml).

Sites built with an older Editor keep calling the server, so a change to the file keeps the calls of earlier Editors working.

## Image

CI builds a static binary on `gcr.io/distroless/static-debian13:nonroot` and starts it before anything is pushed. Pushes to `main` push it to `ghcr.io/rmypg/edity-server`, tagged `sha-<commit>` and `development`. When pushes to `main` arrive faster than CI runs, the commits in between are skipped, and `development` always ends on the latest one. It listens on port 8080 and runs as a non-root user.

## License

[AGPL-3.0](LICENSE), except [`api/openapi.yaml`](api/openapi.yaml), which is [MPL-2.0](api/LICENSE) like the edity packages: it is the contract the Editor's client is generated from, and `@edity/astro` ships that client. The reasons are in [ADR-0006](https://github.com/RMYPG/edity/blob/main/docs/adr/0006-mpl-for-the-packages-agpl-for-the-server.md).
