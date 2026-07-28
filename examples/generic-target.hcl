# Minimal non-CP4D profile: any HTTP API that stores a Vault token. The
# payload shape is whatever the target expects — the template variables are
# .VaultToken, .Accessor, .TTLSeconds, and .Extra.*. Only include a login
# block if the target has a session-exchange endpoint; a static header
# in `headers` covers API-key-style auth.

vault {
  address = "https://vault.example.com:8200"

  auth {
    method = "kubernetes"
    role   = "my-integration"
  }
}

push {
  url    = "https://target.example.com/api/v1/secrets/vault-token"
  method = "PUT"

  headers = {
    "Content-Type" = "application/json"
    "X-Api-Key"    = "replace-me"
  }

  body_template = <<-EOT
    {"token":"{{ .VaultToken }}","ttl_seconds":{{ .TTLSeconds }}}
  EOT
}
