# Configuration design

The v0.2.0 schema is the outcome of a deliberate design review after the
initial audit-driven fixes had grown the config ad hoc. This page records
the shape, the reference it borrows from, and the divergences — so future
additions extend a design rather than accrete onto one.

## The shape: three nouns

```hcl
vault { ... }        # where tokens come from
credentials { ... }  # what authenticates mint4v to the target
target { ... }       # where tokens go, and the shape of the delivery
```

Blocks name *things*, not actions. The delivery block was originally called
`push`; it was renamed because "push" is the verb for what mint4v does,
while everything around the config already used the noun — the chart's
`targetCASecret`, the threat model's "target API". "Push" remains the verb
in prose, logs, and code.

## Vault Agent as the reference

mint4v is structurally a specialised
[Vault Agent](https://developer.hashicorp.com/vault/docs/agent-and-proxy/agent):
auto-auth, then template, then deliver. Its operators are Vault operators,
so where the concepts coincide the config borrows Agent's vocabulary:

- `vault { address, namespace, ca_cert, client_cert, client_key }` —
  Agent's block and attribute names (`ca_cert`, not `ca_cert_file`).
- `auth "kubernetes" { role, mount_path, token_path }` — a labelled block
  selecting the method, echoing Agent's `method "kubernetes"`, with Agent's
  `role`/`token_path` attribute names. A labelled block gives each method
  its own schema instead of one struct of shared optional fields.
- Templates fail on missing keys (`missingkey=error`), matching the intent
  of Agent's `error_on_missing_key`, and are always on — misconfiguration
  should be loud.

## Deliberate divergences

- **`target` is not a `sink`.** Agent's sinks and template destinations
  write secrets to *disk* — the exposure mint4v exists to eliminate. The
  delivery block is the replacement for the sink, not an instance of it, so
  it takes its own shape: an HTTP request description (url, method, headers,
  body template, optional pre-login).
- **Token lifecycle is first-class.** `revoke_grace`, rotation-on-max-TTL,
  and revoke-on-shutdown have no Agent analogue; they live in the `vault`
  block because they are properties of the token mint4v holds.
- **No surface parity.** Agent's cache, proxy, listener, and
  `template_config` blocks solve problems mint4v does not have. The config
  stays deliberately small: if the target needs another value, it goes in
  `target.extra`; if it needs computation, compute it before it reaches the
  config.

## Credentials: one concept, one block

Credential material never sits in the config file (a ConfigMap) — it is
referenced from Secret custody and exposed to templates as
`{{ .Credentials.* }}`, consumed by both `target.headers` values and
`target.login.body_template`. An earlier iteration had two
`credentials_file` attributes (target-level and login-level) held apart by
a mutual-exclusion check; the single top-level block replaced that.

The block is also the seam for planned work: sourcing credentials from
Vault KV (memory-only, no Kubernetes Secret at all) adds an alternative
source *inside* this block rather than a new mechanism beside it. That
change will also move header rendering from once-at-startup to per-push,
since a credential that can rotate under a running pod must be re-read.

## Compatibility

The v0.1.0 → v0.2.0 schema change is breaking (renames: `push` → `target`,
`ca_cert_file` → `ca_cert`, `token_file` → `token_path`; restructure:
`auth` method label, `credentials` block). It was made while the project
had no known production deployments — the cost of carrying the accreted
schema forever outweighed a one-time mechanical migration. The
[examples](../examples/) and the chart's default `values.yaml` show the
current schema in full.
