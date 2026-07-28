// mockcpd is a stand-in for the Cloud Pak for Data Platform API used by the
// e2e suite. It implements the CP4D session exchange (POST
// /icp4d-api/v1/authorize with username/api_key returning a bearer token) and
// the vault update endpoint (PATCH /zen-data/v2/vaults/<urn> expecting
// {"details":{"vault_address":...,"access_token":...}}). Like real CP4D with
// validate_and_save=true, it verifies the pushed token against Vault
// (lookup-self) before accepting it. GET /last exposes the last accepted
// payload for test assertions.
package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	username := os.Getenv("MOCK_USERNAME")
	apiKey := os.Getenv("MOCK_APIKEY")
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	var mu sync.Mutex
	sessions := map[string]bool{}
	var lastPayload []byte
	var lastPushAt time.Time

	httpClient := &http.Client{Timeout: 10 * time.Second}
	if os.Getenv("MOCK_TLS_SKIP_VERIFY") == "true" {
		// Test mock only: allows validate_and_save against a TLS Vault
		// without mounting a CA bundle.
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /icp4d-api/v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			APIKey   string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username != username || body.APIKey != apiKey {
			http.Error(w, `{"message":"invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		buf := make([]byte, 16)
		rand.Read(buf)
		token := hex.EncodeToString(buf)
		mu.Lock()
		sessions[token] = true
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	})

	mux.HandleFunc("PATCH /zen-data/v2/vaults/{urn}", func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		valid := sessions[bearer]
		mu.Unlock()
		if !valid {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body struct {
			Details struct {
				VaultAddress string `json:"vault_address"`
				AccessToken  string `json:"access_token"`
			} `json:"details"`
		}
		if err := json.Unmarshal(payload, &body); err != nil ||
			body.Details.VaultAddress == "" || body.Details.AccessToken == "" {
			http.Error(w, `{"message":"details.vault_address and details.access_token are required"}`, http.StatusBadRequest)
			return
		}

		// validate_and_save: like real CP4D, test the pushed token against
		// the vault it claims to belong to before accepting it.
		if r.URL.Query().Get("validate_and_save") == "true" {
			if err := validateVaultToken(httpClient, body.Details.VaultAddress, body.Details.AccessToken); err != nil {
				log.Printf("rejected push for vault %q: %v", r.PathValue("urn"), err)
				http.Error(w, fmt.Sprintf(`{"message":"vault validation failed: %v"}`, err), http.StatusBadRequest)
				return
			}
		}

		mu.Lock()
		lastPayload = payload
		lastPushAt = time.Now()
		mu.Unlock()
		log.Printf("accepted %s push for vault %q (%d bytes)", r.Method, r.PathValue("urn"), len(payload))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /last", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if lastPayload == nil {
			http.Error(w, "no push received yet", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Push-Time", lastPushAt.Format(time.RFC3339))
		w.Write(lastPayload)
	})

	log.Printf("mockcpd listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// validateVaultToken performs a token lookup-self against the given Vault,
// mirroring CP4D's validate_and_save behaviour.
func validateVaultToken(client *http.Client, vaultAddress, token string) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(vaultAddress, "/")+"/v1/auth/token/lookup-self", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vault returned %s", resp.Status)
	}
	return nil
}
