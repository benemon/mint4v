# CP4D against an external Vault Enterprise: the jwt auth method validates
# the ServiceAccount token cryptographically (JWKS/static pubkeys), so Vault
# never needs to reach the cluster's API server and no auth-delegator RBAC
# is required. The role's bound_subject must name mint4v's identity:
# system:serviceaccount:<namespace>:mint4v.

vault {
  address   = "https://vault.example.com:8200"
  namespace = "team-a"
  ca_cert   = "/etc/mint4v/tls/vault/ca.crt"

  auth "jwt" {
    mount_path = "jwt-ocp"
    role       = "cp4d"
    token_path = "/var/run/secrets/mint4v/token"
  }
}

credentials {
  file = "/etc/mint4v/credentials/credentials.json"
}

target {
  url     = "https://cpd.example.com/zen-data/v2/vaults/1000330999:my-vault?validate_and_save=true"
  method  = "PATCH"
  ca_cert = "/etc/mint4v/tls/target/ca.crt"

  headers = {
    "Content-Type" = "application/json"
  }

  extra = {
    vault_address = "https://vault.example.com:8200"
  }

  body_template = <<-EOT
    {"details":{"vault_address":"{{ .Extra.vault_address }}","access_token":"{{ .VaultToken }}"}}
  EOT

  login {
    url         = "https://cpd.example.com/icp4d-api/v1/authorize"
    token_field = "token"

    body_template = <<-EOT
      {"username":{{ toJSON .Credentials.username }},"api_key":{{ toJSON .Credentials.api_key }}}
    EOT
  }
}
