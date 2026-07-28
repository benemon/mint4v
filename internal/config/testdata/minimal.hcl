vault {
  address = "http://vault.example.com:8200"

  auth {
    method = "kubernetes"
    role   = "cp4d"
  }
}

push {
  url                = "http://cpd.example.com/api"
  body_template = "{{ .VaultToken }}"
}
