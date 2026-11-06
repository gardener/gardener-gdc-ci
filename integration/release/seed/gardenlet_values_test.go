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

package seed

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
)

func generateSeedTestCertPEM(t *testing.T, commonName string, isCA bool) string {
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
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
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

func baseSeedReleaseTestConfig(gdcCAData string) *config.ReleaseTestConfig {
	return &config.ReleaseTestConfig{
		TestShoot: &config.ShootConfig{
			VirtualGarden: &config.VirtualGardenConfig{
				CAData: []byte("garden-ca"),
				Host:   "https://api.virtual-garden.local",
			},
		},
		GDC: &config.GDCConfig{
			Region:                 "us-west6",
			ManagedDNSDomainName:   "sap.gardener.gpcdemolabs.com",
			HarborRegistryURL:      "harbor.org.internal/kvcsi",
			DualZoneBucketLocation: "async-grover-gonzo",
			CAData:                 gdcCAData,
		},
		Seed: &config.SeedConfig{
			Name: "seed-test",
			Network: &config.NetworkConfiguration{
				PodsCIDR:     "100.64.0.0/16",
				ServicesCIDR: "100.88.0.0/16",
			},
		},
		Gardener: &config.GardenerConfig{
			GardenerVersion: "v1.154.1",
		},
	}
}

func TestPrepareGardenletHelmValues(t *testing.T) {
	t.Run("populates string imageVectorOverwrite with caBundle.inline when GDC.CAData is set", func(t *testing.T) {
		leafPEM := generateSeedTestCertPEM(t, "console", false)
		intermediateCAPEM := generateSeedTestCertPEM(t, "GDC Managed Org Internal TLS CA", true)
		rootCAPEM := generateSeedTestCertPEM(t, "GDC Managed ORG ROOT CA", true)
		b64Bundle := base64.StdEncoding.EncodeToString([]byte(leafPEM + intermediateCAPEM + rootCAPEM))

		values, _, err := prepareGardenletHelmValues(baseSeedReleaseTestConfig(b64Bundle))
		if err != nil {
			t.Fatalf("prepareGardenletHelmValues failed: %v", err)
		}

		rawOverwrite, ok := values["imageVectorOverwrite"]
		if !ok {
			t.Fatalf("expected values[\"imageVectorOverwrite\"] to be present")
		}
		overwriteStr, ok := rawOverwrite.(string)
		if !ok {
			t.Fatalf("expected values[\"imageVectorOverwrite\"] to be string for Helm indent helper, got %T", rawOverwrite)
		}

		var parsed struct {
			Images   []map[string]interface{} `yaml:"images"`
			CABundle struct {
				Inline string `yaml:"inline"`
			} `yaml:"caBundle"`
		}
		if err := yaml.Unmarshal([]byte(overwriteStr), &parsed); err != nil {
			t.Fatalf("failed to unmarshal imageVectorOverwrite string: %v\ncontent:\n%s", err, overwriteStr)
		}
		if len(parsed.Images) != 0 {
			t.Errorf("got %d images, want 0", len(parsed.Images))
		}
		wantInline := intermediateCAPEM + rootCAPEM
		if parsed.CABundle.Inline != wantInline {
			t.Errorf("caBundle.inline mismatch:\ngot:\n%s\nwant:\n%s", parsed.CABundle.Inline, wantInline)
		}
	})

	t.Run("omits imageVectorOverwrite when GDC.CAData is empty", func(t *testing.T) {
		values, _, err := prepareGardenletHelmValues(baseSeedReleaseTestConfig(""))
		if err != nil {
			t.Fatalf("prepareGardenletHelmValues failed: %v", err)
		}
		if _, exists := values["imageVectorOverwrite"]; exists {
			t.Errorf("expected imageVectorOverwrite to be omitted when GDC.CAData is empty, got %v", values["imageVectorOverwrite"])
		}
	})
}
