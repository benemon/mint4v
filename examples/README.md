# Configuration examples

Reference configurations for common usage profiles — the HCL equivalent of
CRD samples. They are not deployable as-is: hostnames, URNs, roles, and
credentials are placeholders, and file paths assume the Helm chart's mounts
(`/etc/mint4v/...`, `/var/run/secrets/mint4v/token`). See the
[configuration reference](../README.md#configuration-reference).

Every example is parse-validated by the unit test suite, so they cannot
drift from the config schema.

| Example | Profile |
|---------|---------|
| [cp4d-zenapikey.hcl](cp4d-zenapikey.hcl) | CP4D, static `ZenApiKey` authentication, kubernetes auth (reviewer JWT model) |
| [cp4d-session-login.hcl](cp4d-session-login.hcl) | CP4D, Bearer session exchange via `/icp4d-api/v1/authorize`, kubernetes auth |
| [cp4d-jwt-external-vault.hcl](cp4d-jwt-external-vault.hcl) | CP4D, jwt auth against an external Vault Enterprise with namespaces |
| [generic-target.hcl](generic-target.hcl) | Non-CP4D HTTP target, minimal configuration |
