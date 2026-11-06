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

package gardener

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func generateTestCertPEM(t *testing.T, commonName string, isCA bool, notBefore, notAfter time.Time) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	keyUsage := x509.KeyUsageDigitalSignature
	if isCA {
		keyUsage |= x509.KeyUsageCertSign
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestBuildGardenletImageVectorOverwrite(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	leafPEM := generateTestCertPEM(t, "console", false, now.Add(-time.Hour), now.Add(24*time.Hour))
	intermediateCAPEM := generateTestCertPEM(t, "GDC Managed Org Internal TLS CA", true, now.Add(-time.Hour), now.Add(365*24*time.Hour))
	rootCAPEM := generateTestCertPEM(t, "GDC Managed ORG ROOT CA", true, now.Add(-time.Hour), now.Add(10*365*24*time.Hour))
	expiredCAPEM := generateTestCertPEM(t, "Expired CA", true, now.Add(-48*time.Hour), now.Add(-time.Hour))

	t.Run("empty CA data returns empty string", func(t *testing.T) {
		got, err := buildGardenletImageVectorOverwriteAt("   ", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
	})

	t.Run("filters leaf and expired certs and preserves all valid CAs from base64 bundle", func(t *testing.T) {
		fullChain := leafPEM + intermediateCAPEM + expiredCAPEM + rootCAPEM
		b64Chain := base64.StdEncoding.EncodeToString([]byte(fullChain))

		got, err := buildGardenletImageVectorOverwriteAt(b64Chain, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "images: []") {
			t.Errorf("expected explicit 'images: []' in YAML, got:\n%s", got)
		}

		var parsed struct {
			Images   []map[string]interface{} `yaml:"images"`
			CABundle struct {
				Inline string `yaml:"inline"`
			} `yaml:"caBundle"`
		}
		if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("failed to unmarshal generated YAML: %v", err)
		}
		if len(parsed.Images) != 0 {
			t.Errorf("got %d images, want 0", len(parsed.Images))
		}
		wantPEM := intermediateCAPEM + rootCAPEM
		if parsed.CABundle.Inline != wantPEM {
			t.Errorf("caBundle.inline mismatch:\ngot:\n%s\nwant:\n%s", parsed.CABundle.Inline, wantPEM)
		}
	})

	t.Run("accepts raw PEM input directly", func(t *testing.T) {
		got, err := buildGardenletImageVectorOverwriteAt(rootCAPEM, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "-----BEGIN CERTIFICATE-----") {
			t.Errorf("expected PEM block in output, got:\n%s", got)
		}
	})

	t.Run("errors on invalid base64", func(t *testing.T) {
		if _, err := buildGardenletImageVectorOverwriteAt("!!!not-base64!!!", now); err == nil {
			t.Fatal("expected error for invalid base64, got nil")
		}
	})

	t.Run("errors when bundle has no valid non-expired CA certificates", func(t *testing.T) {
		onlyLeafAndExpired := base64.StdEncoding.EncodeToString([]byte(leafPEM + expiredCAPEM))
		if _, err := buildGardenletImageVectorOverwriteAt(onlyLeafAndExpired, now); err == nil {
			t.Fatal("expected error when no valid CA certs exist, got nil")
		}
	})
}
