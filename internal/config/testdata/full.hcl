log_level      = "debug"
health_address = ":9090"

vault {
  address      = "https://vault.example.com:8200"
  namespace    = "admin/infra"
  ca_cert_file = "/etc/minter/tls/vault-ca.pem"
  revoke_grace = "10s"

  auth {
    method     = "jwt"
    mount_path = "jwt-ocp"
    role       = "cp4d"
    token_file = "/var/run/secrets/minter/token"
  }
}

push {
  url                = "https://cpd.example.com/zen-data/v2/vaults/1"
  method             = "put"
  ca_cert_file       = "/etc/minter/tls/target-ca.pem"
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
    url                = "https://cpd.example.com/icp4d-api/v1/authorize"
    body_template = <<-EOT
      {"username":"{{ .Credentials.username }}"}
    EOT
    token_field        = "token"
    credentials_file   = "/etc/minter/credentials/credentials.json"
  }
}
