[![CI](https://github.com/benemon/mint4v/actions/workflows/ci.yml/badge.svg)](https://github.com/benemon/mint4v/actions/workflows/ci.yml)
[![E2E](https://github.com/benemon/mint4v/actions/workflows/e2e.yml/badge.svg)](https://github.com/benemon/mint4v/actions/workflows/e2e.yml)

# mint4v

<img src="assets/logo.svg" alt="a hand-drawn mint with a hole in it" width="180" align="right"/>

Token Minter for Vault. mint4v replaces the long-lived Vault token stored in
IBM Cloud Pak for Data's external-vault integration with a short-lived,
automatically rotated one. It runs as a single-replica Deployment: it logs in
to Vault with the Kubernetes ServiceAccount identity it runs as, pushes the
resulting service token to CP4D's vault-update API, renews the token in
place, rotates it at max TTL, and revokes it on shutdown. The token exists
only in process memory.

## Architectural boundaries

**A token broker, not an operator.** mint4v is one binary, one HCL config
file, one Deployment. There are no CRDs and no reconcile loop — the Helm
chart is the whole deployment surface. If the process dies, Kubernetes
restarts it and it mints and pushes a fresh token; that restart path is the
recovery mechanism, and nothing else is.

**CP4D is the integration; the machinery is generic.** The push side is a
templated HTTP request (Go `text/template`), so any API that stores a Vault
token can be a target, but CP4D is what the defaults, the documentation, and
the e2e suite are built around. The mock CP4D used in e2e implements the real
contract — `/icp4d-api/v1/authorize` and `PATCH /zen-data/v2/vaults/<urn>`,
including token validation against Vault when `validate_and_save=true`.

**Exactly one replica.** A second minter would push competing tokens and
revoke its peer's. The chart hardcodes `replicas: 1` with a
[`Recreate` strategy](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#recreate-deployment)
so the old pod revokes before the new one logs in. There is no HA mode and
none is planned: the failure window is bounded by pod restart time, during
which CP4D's existing token keeps working.

## How it works

```
 ServiceAccount JWT ──► Vault login ──► push token to CP4D
                            │                    ▲
                            ▼                    │ (only at rotation)
                     renew in place ── max TTL ──┘── revoke old after grace
```

The first login and push fail fast, so misconfiguration surfaces as a crash
loop. During rotation, failures are retried with backoff while the previous
token drains; the old token is never revoked until its replacement has been
pushed successfully. On SIGTERM the current token is revoked before exit. On
crash without revocation, the token dies at its TTL — `token_ttl` on the
Vault role is the bound on an orphaned token's lifetime, and
`token_max_ttl` sets the rotation cadence.

Batch tokens are refused at startup — renewal and revocation require
renewable
[service tokens](https://developer.hashicorp.com/vault/docs/concepts/tokens#service-tokens).

## Prerequisites

- Go 1.26+ (development only)
- Docker, kind, and Helm (e2e only)
- A Vault auth role (`kubernetes` or `jwt` method) bound to the minter's
  ServiceAccount
- A CP4D user with vault-administration permission

## Getting started

Build and deploy:

```sh
make docker-build IMG=<registry>/mint4v:<tag>
helm install mint4v charts/mint4v -n <namespace> -f my-values.yaml
```

The configuration is one HCL file, templates included. The chart mounts
everything it references at fixed paths: the config from a ConfigMap at
`/etc/minter/config.hcl`, optional CA-bundle Secrets at
`/etc/minter/tls/vault/` and `/etc/minter/tls/target/`, an optional
credentials Secret at `/etc/minter/credentials/`, and a
[projected ServiceAccount token](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken)
at `/var/run/secrets/minter/token`. Each mounted item takes either the name
of an existing Secret (`vaultCASecret`, `targetCASecret`,
`credentialsSecret`) or an inline literal (`vaultCA`, `targetCA`,
`credentials`) that the chart materializes as a Secret itself — the two
forms are mutually exclusive per item, and inline credentials end up in the
values file and Helm release Secret, so prefer the existing-Secret form for
anything beyond a lab. The pod satisfies the
[restricted Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted)
with the default SA token mount disabled — compatible with OpenShift's
`restricted-v2` SCC. See the comments in
[charts/mint4v/values.yaml](charts/mint4v/values.yaml) for the full values
reference.

## Configuration reference

One HCL file, passed as `-config /path/to/config.hcl`.

### Top level

| Field | Default | Description |
|-------|---------|-------------|
| `log_level` | `info` | `debug`, `info`, `warn`, or `error`. Logs never contain the token — accessors only |
| `health_address` | `:8080` | Listen address for `/healthz`, used by the chart's probes |

### `vault`

| Field | Default | Description |
|-------|---------|-------------|
| `address` | | Vault address (required) |
| `namespace` | | [Vault Enterprise namespace](https://developer.hashicorp.com/vault/docs/enterprise/namespaces) |
| `ca_cert_file` | | PEM bundle appended to the system pool for the Vault connection, mounted from a Secret (`vaultCASecret` in the chart) |
| `revoke_grace` | `30s` | How long the old token outlives its replacement after a rotation push |

### `vault.auth`

| Field | Default | Description |
|-------|---------|-------------|
| `method` | | [`kubernetes`](https://developer.hashicorp.com/vault/docs/auth/kubernetes) or [`jwt`](https://developer.hashicorp.com/vault/docs/auth/jwt) (required) |
| `role` | | Vault auth role (required) |
| `mount_path` | method name | Auth mount path |
| `token_file` | in-pod SA token path | ServiceAccount token file. The chart mounts a [projected token](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken) at `/var/run/secrets/minter/token` |

### `push`

| Field | Default | Description |
|-------|---------|-------------|
| `url` | | Target URL (required). For CP4D: `https://<host>/zen-data/v2/vaults/<urn>?validate_and_save=true` |
| `method` | `POST` | HTTP method. CP4D vault updates use `PATCH` |
| `ca_cert_file` | | PEM bundle for the target connection, mounted from a Secret (`targetCASecret` in the chart). Deliberately a separate pool from the Vault CA |
| `body_template` | | Inline payload template, usually an [HCL heredoc](https://developer.hashicorp.com/terraform/language/expressions/strings#heredoc-strings) (required) |
| `headers` | | Static request headers |
| `extra` | | Static key/values exposed to the payload template as `{{ .Extra.* }}` |

### `push.login` (optional block)

Omit the block entirely if the target needs no authentication or a static
header suffices.

| Field | Default | Description |
|-------|---------|-------------|
| `url` | | Login endpoint. For CP4D: `https://<host>/icp4d-api/v1/authorize` |
| `body_template` | | Inline login body template, rendered with `{{ .Credentials.* }}` |
| `token_field` | | Dot-path to the bearer token in the JSON response (`token` for CP4D) |
| `credentials_file` | | JSON object mounted from a Secret, exposed as `{{ .Credentials.* }}` |

### Template variables

Templates are inline Go [`text/template`](https://pkg.go.dev/text/template)
strings. Payload template: `{{ .VaultToken }}`, `{{ .Accessor }}`,
`{{ .TTLSeconds }}`, `{{ .Extra.* }}`. Login template:
`{{ .Credentials.* }}`. There is no syntax collision — Go templates
interpolate with `{{ }}`, HCL with `${ }`; a literal `${` in a payload must
be written `$${`. Templates run with `missingkey=error`; execution errors
are logged without the data being rendered.

Secrets, CA bundles, and the ServiceAccount token deliberately stay file
references rather than inlining: the config lives in a ConfigMap, so
inlining a credential would move it out of Secret custody, and certificates
arrive in Kubernetes as mounted files.

## Cloud Pak for Data integration

Verified against the CP4D 5.x Platform API. A `hashicorp_token` vault
integration is updated with:

```
PATCH https://<cpd-host>/zen-data/v2/vaults/<vault_urn>?validate_and_save=true
Authorization: ZenApiKey <base64(username:apikey)>   (or: Bearer <session token>)
Content-Type: application/json

{"details": {"vault_address": "<vault-address>", "access_token": "<vault-token>"}}
```

`vault_urn` is `<creator uid>:<vault_name>`. With `validate_and_save=true`,
CP4D tests the pushed token against Vault before saving it, so every
rotation is verified end to end. The matching config:

```hcl
push {
  url     = "https://cpd.example.com/zen-data/v2/vaults/1000330999:my-vault?validate_and_save=true"
  method  = "PATCH"
  headers = { "Content-Type" = "application/json" }
  extra   = { vault_address = "https://vault.example.com:8200" }

  body_template = <<-EOT
    {"details":{"vault_address":"{{ .Extra.vault_address }}","access_token":"{{ .VaultToken }}"}}
  EOT
}
```

CP4D accepts two authentication styles, both supported:

- **ZenApiKey** (static): omit the `login` block; add
  `Authorization = "ZenApiKey <base64 of username:apikey>"` to `push.headers`.
- **Bearer session token**: point the `login` block at
  `/icp4d-api/v1/authorize` with `token_field = "token"`. The exchange runs
  before each push; pushes happen once per rotation, so session-token expiry
  is irrelevant.

### Sourcing the CP4D credential

The CP4D credential is the one long-lived secret left in the design. Store
it in Vault and sync it with the
[Vault Secrets Operator](https://developer.hashicorp.com/vault/docs/platform/k8s/vso):
a [`VaultStaticSecret`](https://developer.hashicorp.com/vault/docs/platform/k8s/vso/api-reference#vaultstaticsecret)
renders the KV entry into a Kubernetes Secret with a `credentials.json`
key, and `credentialsSecret` in the chart values points at it. There is no circular dependency — this credential authenticates
mint4v *to CP4D*; the token CP4D uses *for Vault* is the one mint4v mints.
The credential is read at startup, so a rotated Secret takes effect on the
next pod restart.

## Vault setup

The auth role must issue renewable
[service tokens](https://developer.hashicorp.com/vault/docs/concepts/tokens#service-tokens);
`token_ttl` and `token_max_ttl` semantics are documented under
[token TTLs](https://developer.hashicorp.com/vault/docs/concepts/tokens#token-time-to-live-periodic-tokens-and-explicit-max-ttls).
Suggested starting point — 15m TTL, 24h rotation:

```sh
vault write auth/kubernetes/role/cp4d \
    bound_service_account_names=mint4v \
    bound_service_account_namespaces=<namespace> \
    audience=vault \
    token_policies=cp4d-read \
    token_ttl=15m token_max_ttl=24h
```

The policy attached to this role is the blast radius of the whole system —
scope it to exactly the paths CP4D reads.

For the [`jwt` method](https://developer.hashicorp.com/vault/docs/auth/jwt),
Vault validates the SA token against the cluster's OIDC issuer instead of
calling TokenReview — use it when Vault cannot reach the API server. One
caveat worth stating here because it is easy to trip on:
`oidc_discovery_url` must exactly match the cluster's issuer (e.g.
`https://kubernetes.default.svc.cluster.local`, not `…svc`).

### Who needs `system:auth-delegator`

The [`kubernetes` auth method](https://developer.hashicorp.com/vault/docs/auth/kubernetes)
validates tokens via the
[TokenReview API](https://kubernetes.io/docs/reference/kubernetes-api/authentication-resources/token-review-v1/),
and the identity making that call needs the
[`system:auth-delegator`](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#other-component-roles)
ClusterRole. Which identity that is depends on the mount configuration
(see [`disable_local_ca_jwt`](https://developer.hashicorp.com/vault/api-docs/auth/kubernetes#disable_local_ca_jwt)):

| Mount configuration | TokenReview caller | Grant `auth-delegator` to | Client credential |
|---------------------|--------------------|---------------------------|-------------------|
| `token_reviewer_jwt` configured | that JWT's identity | the reviewer's ServiceAccount | projected token, `audience=vault` (chart default) |
| No reviewer JWT; Vault in-cluster | Vault's own pod ServiceAccount | Vault's pod ServiceAccount | any, including a [legacy Secret token](https://kubernetes.io/docs/concepts/configuration/secret/#service-account-token-secrets) (`tokenSecretName` in the chart) |
| No reviewer JWT; Vault external (or `disable_local_ca_jwt=true`) | the client's login JWT | **mint4v's** ServiceAccount | must be API-server-valid: set `tokenAudience: ""` in the chart |
| `jwt` auth method | nobody (JWKS signature check) | nobody | projected token, audience per the role |

The caveat in the third row is load-bearing: a `vault`-audience JWT cannot
authenticate to the API server to perform its own TokenReview. All four
rows are exercised by the e2e suite.

## Development and testing

```sh
make test        # unit tests (fake Vault + httptest targets)
make lint        # golangci-lint
make helm-lint   # chart lint
make docker-build IMG=mint4v:dev
make test-e2e    # KIND + real Vault + mock CP4D
```

The e2e suite creates a kind cluster, deploys a dev-mode Vault and the mock
CP4D API ([test/mockcpd](test/mockcpd/)), installs the chart, and asserts
with deliberately short TTLs (1m/2m): the token is pushed and valid, renewed
in place past its initial TTL without a re-push, rotated at max TTL with the
old token revoked after the grace period, and revoked when the Deployment is
scaled down — across all three kubernetes-auth TokenReview models and the
jwt method.

CI: [ci.yml](.github/workflows/ci.yml) runs lint, unit tests, and chart
checks on pushes to main and PRs; [e2e.yml](.github/workflows/e2e.yml) runs
the full KIND suite on PRs. Both support `workflow_dispatch`. There is no
release pipeline.

## Threat model

See [docs/threat-model.md](docs/threat-model.md) — structured after the
Vault Secrets Operator threat model, covering the minted token's custody,
the credentials mint4v consumes, and recommendations for secure deployment.
