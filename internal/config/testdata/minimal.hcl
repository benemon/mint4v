vault {
  address = "http://vault.example.com:8200"

  auth "kubernetes" {
    role = "cp4d"
  }
}

target {
  url           = "http://cpd.example.com/api"
  body_template = "{{ .VaultToken }}"
}
