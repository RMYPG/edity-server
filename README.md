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

Run the checks CI runs, the Go and workflow linters and the tests, with:

```bash
mise run ci
```

`mise run dev` starts the server on `localhost:8080` with the `dev` secrets of the `edity` Infisical project, which it reads as your own Infisical user: log in once with `infisical login`. It serves the Sites in [`sites.dev.json`](sites.dev.json), where `edity-sandbox` answers on `localhost`.

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

`GET /healthz` answers Kubernetes probes on any hostname. The routes of the spec's API answer 501 until they are implemented. Logs are JSON lines on stdout, one per request.

## Image

CI builds a static binary on `gcr.io/distroless/static-debian13:nonroot` and starts it before anything is pushed. Pushes to `main` push it to `ghcr.io/rmypg/edity-server`, tagged `sha-<commit>` and `development`. When pushes to `main` arrive faster than CI runs, the commits in between are skipped, and `development` always ends on the latest one. It listens on port 8080 and runs as a non-root user.

## License

[AGPL-3.0](LICENSE). The reason is in [ADR-0006](https://github.com/RMYPG/edity/blob/main/docs/adr/0006-mpl-for-the-packages-agpl-for-the-server.md).
