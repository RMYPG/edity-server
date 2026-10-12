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
