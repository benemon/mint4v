# CP4D with the Bearer session exchange: mint4v POSTs username/api_key to
# /icp4d-api/v1/authorize before each push (pushes happen once per rotation)
# and uses the returned token as the Authorization bearer. The credential is
# mounted from a Secret — see `credentialsSecret`/`credentials` in the chart
# values, ideally synced from Vault by the Vault Secrets Operator.

vault {
  address      = "https://vault.example.com:8200"
  ca_cert_file = "/etc/minter/tls/vault/ca.crt"
  revoke_grace = "30s"

  auth {
    method = "kubernetes"
    role   = "cp4d"
  }
}

push {
  url    = "https://cpd.example.com/zen-data/v2/vaults/1000330999:my-vault?validate_and_save=true"
  method = "PATCH"

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
      {"username":"{{ .Credentials.username }}","api_key":"{{ .Credentials.api_key }}"}
    EOT

    credentials_file = "/etc/minter/credentials/credentials.json"
  }
}
