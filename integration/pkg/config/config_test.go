// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchGDCCADataVerifiesTLSAndSupportsBootstrapCA(t *testing.T) {
	origDelay := fetchCADataRetryDelay
	fetchCADataRetryDelay = 0
	t.Cleanup(func() { fetchCADataRetryDelay = origDelay })

	wantBody := "-----BEGIN CERTIFICATE-----\nFAKEBUNDLE\n-----END CERTIFICATE-----\n"
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/certificate-authority" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer srv.Close()

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: srv.Certificate().Raw,
	})

	t.Run("rejects untrusted certificate without GDC_BOOTSTRAP_CA_PEM", func(t *testing.T) {
		t.Setenv("GDC_BOOTSTRAP_CA_PEM", "")
		if _, err := FetchGDCCAData(srv.URL); err == nil {
			t.Fatal("expected TLS verification error for self-signed server without GDC_BOOTSTRAP_CA_PEM")
		}
	})

	t.Run("rejects invalid PEM in GDC_BOOTSTRAP_CA_PEM", func(t *testing.T) {
		t.Setenv("GDC_BOOTSTRAP_CA_PEM", "not a pem certificate")
		if _, err := FetchGDCCAData(srv.URL); err == nil {
			t.Fatal("expected error for invalid PEM in GDC_BOOTSTRAP_CA_PEM")
		}
	})

	t.Run("succeeds with matching GDC_BOOTSTRAP_CA_PEM", func(t *testing.T) {
		t.Setenv("GDC_BOOTSTRAP_CA_PEM", string(certPEM))
		gotB64, err := FetchGDCCAData(srv.URL)
		if err != nil {
			t.Fatalf("FetchGDCCAData failed with trusted bootstrap CA: %v", err)
		}
		gotBytes, err := base64.StdEncoding.DecodeString(gotB64)
		if err != nil {
			t.Fatalf("returned value is not valid base64: %v", err)
		}
		if string(gotBytes) != wantBody {
			t.Fatalf("decoded CA bundle = %q, want %q", string(gotBytes), wantBody)
		}
	})

	t.Run("rejects non-200 response", func(t *testing.T) {
		t.Setenv("GDC_BOOTSTRAP_CA_PEM", string(certPEM))
		status = http.StatusServiceUnavailable
		t.Cleanup(func() { status = http.StatusOK })
		if _, err := FetchGDCCAData(srv.URL); err == nil {
			t.Fatal("expected error for HTTP 503 response")
		}
	})
}
