# CP4D with static ZenApiKey authentication — no pre-login exchange. The
# ZenApiKey value is base64(username:apikey) and lives in the Secret-mounted
# credentials file ({"zen_api_key": "..."}), referenced from the header
# template; the config itself (a ConfigMap) carries no credential material.
# Vault side: kubernetes auth with a reviewer JWT configured on the mount.

vault {
  address = "https://vault.example.com:8200"
  ca_cert = "/etc/mint4v/tls/vault/ca.crt"

  auth "kubernetes" {
    role = "cp4d"
  }
}

credentials {
  file = "/etc/mint4v/credentials/credentials.json"
}

target {
  url    = "https://cpd.example.com/zen-data/v2/vaults/1000330999:my-vault?validate_and_save=true"
  method = "PATCH"

  headers = {
    "Content-Type"  = "application/json"
    "Authorization" = "ZenApiKey {{ .Credentials.zen_api_key }}"
  }

  extra = {
    vault_address = "https://vault.example.com:8200"
  }

  body_template = <<-EOT
    {"details":{"vault_address":"{{ .Extra.vault_address }}","access_token":"{{ .VaultToken }}"}}
  EOT
}
