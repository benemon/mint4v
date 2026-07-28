# Minimal non-CP4D profile: any HTTP API that stores a Vault token. The
# payload shape is whatever the target expects — the template variables are
# .VaultToken, .Accessor, .TTLSeconds, and .Extra.*. Only include a login
# block if the target has a session-exchange endpoint; an API-key-style
# header is templated from the Secret-mounted credentials file so the key
# never sits in the ConfigMap-resident config.

vault {
  address = "https://vault.example.com:8200"

  auth "kubernetes" {
    role = "my-integration"
  }
}

credentials {
  file = "/etc/mint4v/credentials/credentials.json"
}

target {
  url    = "https://target.example.com/api/v1/secrets/vault-token"
  method = "PUT"

  headers = {
    "Content-Type" = "application/json"
    "X-Api-Key"    = "{{ .Credentials.api_key }}"
  }

  body_template = <<-EOT
    {"token":"{{ .VaultToken }}","ttl_seconds":{{ .TTLSeconds }}}
  EOT
}
