log_level      = "debug"
health_address = ":9090"

vault {
  address      = "https://vault.example.com:8200"
  namespace    = "admin/infra"
  ca_cert      = "/etc/mint4v/tls/vault-ca.pem"
  revoke_grace = "10s"

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
  url           = "https://cpd.example.com/zen-data/v2/vaults/1"
  method        = "put"
  ca_cert       = "/etc/mint4v/tls/target-ca.pem"
  body_template = <<-EOT
    {"details":{"vault_address":"{{ .Extra.vault_id }}","access_token":"{{ .VaultToken }}"}}
  EOT

  headers = {
    "Content-Type" = "application/json"
  }

  extra = {
    vault_id = "1"
  }

  login {
    url           = "https://cpd.example.com/icp4d-api/v1/authorize"
    body_template = <<-EOT
      {"username":{{ toJSON .Credentials.username }}}
    EOT
    token_field   = "token"
  }
}
