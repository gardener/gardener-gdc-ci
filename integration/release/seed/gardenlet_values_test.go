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
	"context"
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
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestDeleteTargetShootVPAResources(t *testing.T) {
	s := runtime.NewScheme()
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add rbacv1 scheme: %v", err)
	}
	if err := admissionregistrationv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add admissionregistrationv1 scheme: %v", err)
	}

	grmCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "gardener.cloud:target:resource-manager"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "gardener-resource-manager",
				Namespace: "kube-system",
			},
		},
	}
	vpaActorCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "gardener.cloud:vpa:target:actor"},
	}
	vpaWebhook := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "vpa-webhook-config-target"},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(grmCRB, vpaActorCRB, vpaWebhook).Build()
	ctx := context.Background()

	deleteTargetShootVPAResources(ctx, fakeClient)

	// Verify gardener.cloud:target:resource-manager and its subjects remain intact so token-requestor keeps working.
	gotGRM := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: "gardener.cloud:target:resource-manager"}, gotGRM); err != nil {
		t.Fatalf("expected gardener.cloud:target:resource-manager to remain present, got error: %v", err)
	}
	if len(gotGRM.Subjects) != 1 || gotGRM.Subjects[0].Name != "gardener-resource-manager" {
		t.Errorf("expected gardener.cloud:target:resource-manager subjects to remain intact, got %+v", gotGRM.Subjects)
	}

	// Verify target VPA CRB and webhook were deleted.
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: "gardener.cloud:vpa:target:actor"}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected gardener.cloud:vpa:target:actor to be deleted, got err=%v", err)
	}
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: "vpa-webhook-config-target"}, &admissionregistrationv1.MutatingWebhookConfiguration{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected vpa-webhook-config-target to be deleted, got err=%v", err)
	}
}
