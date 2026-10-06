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
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	gardencorev1 "github.com/gardener/gardener/pkg/apis/core/v1"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	securityv1alpha1 "github.com/gardener/gardener/pkg/apis/security/v1alpha1"
	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	globalobjectv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/object/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
	gdchnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	objectv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/object/v1"
	resourcemanagerv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/resourcemanager/v1"
	resourcemanagerv1alpha1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/resourcemanager/v1alpha1"
	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdch"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdcloud"
)

var (
	globalSchema     = runtime.NewScheme()
	managementSchema = runtime.NewScheme()
)

// RegisterGDCSchemes registers all GDC/GDCH APIs to the given scheme.
func RegisterGDCSchemes(s *runtime.Scheme) error {
	if err := scheme.AddToScheme(s); err != nil {
		return err
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		return err
	}
	if err := operatorv1alpha1.AddToScheme(s); err != nil {
		return err
	}
	if err := gardencorev1beta1.AddToScheme(s); err != nil {
		return err
	}
	if err := ipamglobalv1.AddToScheme(s); err != nil {
		return err
	}
	if err := globalnetworkingv1.AddToScheme(s); err != nil {
		return err
	}
	if err := globalobjectv1.AddToScheme(s); err != nil {
		return err
	}
	if err := ipamv1.AddToScheme(s); err != nil {
		return err
	}
	if err := gdchnetworkingv1.AddToScheme(s); err != nil {
		return err
	}
	if err := objectv1.AddToScheme(s); err != nil {
		return err
	}
	if err := resourcemanagerv1.AddToScheme(s); err != nil {
		return err
	}
	if err := resourcemanagerv1alpha1.AddToScheme(s); err != nil {
		return err
	}
	if err := vmv1.AddToScheme(s); err != nil {
		return err
	}
	return nil
}

func init() {
	utilruntime.Must(RegisterGDCSchemes(globalSchema))
	utilruntime.Must(RegisterGDCSchemes(managementSchema))
}

// AccessLatestSecret fetches a secret from environment variables or local credential files
// injected by GitHub Actions Secrets.
// This function is defined as a variable to allow mocking in unit tests.
var AccessLatestSecret = func(ctx context.Context, secretName string) ([]byte, error) {
	switch secretName {
	case "gdc-service-account", "staging-gdc-service-account-keys":
		if val := os.Getenv("GDC_RELEASE_SERVICE_ACCOUNT_KEY"); val != "" {
			return []byte(val), nil
		}
		if path := os.Getenv("GDC_SERVICE_ACCOUNT_FILE"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("failed to read GDC_SERVICE_ACCOUNT_FILE %q: %w", path, err)
			}
			return data, nil
		}
		return nil, fmt.Errorf("GDC service account credentials not set; set GDC_RELEASE_SERVICE_ACCOUNT_KEY or GDC_SERVICE_ACCOUNT_FILE")
	case "harbor-docker-config", "staging-harbor-docker-config-credentials":
		if val := os.Getenv("GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON"); val != "" {
			return []byte(val), nil
		}
		if path := os.Getenv("HARBOR_DOCKER_CONFIG_FILE"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("failed to read HARBOR_DOCKER_CONFIG_FILE %q: %w", path, err)
			}
			return data, nil
		}
		return nil, fmt.Errorf("Harbor docker config credentials not set; set GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON or HARBOR_DOCKER_CONFIG_FILE")
	default:
		return nil, fmt.Errorf("unknown secret %q", secretName)
	}
}

// WriteToFile writes the given data to a temporary file with the given pattern
// and returns the path to that file. The file is closed after writing, and if a write error occurs,
// the file is removed.
func WriteToFile(pattern string, data []byte) (string, error) {
	tmpFile, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to close temp file %s: %w", tmpFile.Name(), err)
	}

	return tmpFile.Name(), nil
}

// FetchGDCCAData fetches the CA data from the console URL and returns it as a
// base64 encoded string.
var FetchGDCCAData = func(consoleURL string) (string, error) {
	caDataURL := fmt.Sprintf("%s/.well-known/certificate-authority", consoleURL)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{
		Transport: tr,
		Timeout:   15 * time.Second,
	}
	var lastErr error
	for attempt := 1; attempt <= 6; attempt++ {
		resp, err := httpClient.Get(caDataURL)
		if err != nil {
			lastErr = fmt.Errorf("failed to fetch CA data from %s: %w", caDataURL, err)
			if attempt < 6 {
				time.Sleep(5 * time.Second)
			}
			continue
		}
		caData, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("failed to read CA data: %w", readErr)
			if attempt < 6 {
				time.Sleep(5 * time.Second)
			}
			continue
		}
		return base64.StdEncoding.EncodeToString(caData), nil
	}
	return "", lastErr
}

// GetGDCUserClusterClients creates and returns clients for the specified GDC user clusters.
// This function is a variable to allow for mocking in unit tests.
var GetGDCUserClusterClients = func(userClusters []RuntimeClusterConfig, gdcClient *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
	userClusterSchema := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(userClusterSchema); err != nil {
		return nil, fmt.Errorf("failed to register operatorv1alpha1 scheme: %w", err)
	}
	if err := corev1.AddToScheme(userClusterSchema); err != nil {
		return nil, fmt.Errorf("failed to register corev1 scheme: %w", err)
	}
	if err := RegisterGDCSchemes(userClusterSchema); err != nil {
		return nil, fmt.Errorf("failed to register GDC schemes: %w", err)
	}
	userClusterClients := make(map[string]client.WithWatch)
	for _, userCluster := range userClusters {
		userClusterClient, err := gdch.GetUserClusterClient(gdcClient, userCluster.Zone, userCluster.Project, userCluster.Name, userClusterSchema)

		if err != nil {
			return nil, fmt.Errorf("unable to get client for user cluster %s; %w\n", userCluster.Name, err)
		}
		userClusterClients[userCluster.Name] = userClusterClient
	}
	return userClusterClients, nil
}

// GetGlobalClient creates a client to the Global API server.
// This function is defined as a variable to allow mocking in unit tests.
var GetGlobalClient = func(gdcClient *gdcloud.TestingClient) (client.WithWatch, error) {
	globalClient, err := gdch.GetGlobalClient(gdcClient, globalSchema)
	if err != nil {
		return nil, fmt.Errorf("cannot create client for Global API %w", err)
	}
	return globalClient, nil
}

// GetManagementClient creates a client to the Management API server.
var GetManagementClient = func(gdcloudClient *gdcloud.TestingClient, orgName, zone string) (client.WithWatch, error) {
	return gdch.GetManagementClient(gdcloudClient, orgName, zone, managementSchema)
}

// GetGardenClient retrieves the garden kubeconfig from a secret in the user cluster,
// then uses it to create and return a garden client.
// This function is a variable to allow for mocking in unit tests.
var GetGardenClient = func(ctx context.Context, userClusterClient client.Client) (*VirtualGardenConfig, error) {
	secret := &corev1.Secret{}
	var getErr error
	pollGetErr := wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		getErr = userClusterClient.Get(
			ctx,
			client.ObjectKey{
				Namespace: "garden",
				Name:      "gardener",
			},
			secret,
		)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return false, getErr
			}
			return false, nil
		}
		return true, nil
	})
	if pollGetErr != nil {
		if getErr != nil {
			return nil, fmt.Errorf("failed to get gardener secret: %w", getErr)
		}
		return nil, fmt.Errorf("failed to get gardener secret: %w", pollGetErr)
	}

	kubeconfigData, exists := secret.Data["kubeconfig"]
	if !exists {
		return nil, fmt.Errorf("kubeconfig not found in secret data")
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from kubeconfig content: %w", err)
	}

	s := scheme.Scheme
	if err := gardencorev1beta1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("failed to add gardencorev1beta1 scheme: %w", err)
	}
	if err := gardencorev1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("failed to add gardencorev1 scheme: %w", err)
	}
	if err := securityv1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("failed to add securityv1alpha1 scheme: %w", err)
	}

	gardenClient, err := client.NewWithWatch(restConfig, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("failed to create garden client: %w", err)
	}

	// Verify connection to virtual-garden API server, retrying on transient downtime/restarts
	var lastErr error
	pollErr := wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		nsList := &corev1.NamespaceList{}
		if err := gardenClient.List(ctx, nsList, client.Limit(1)); err != nil {
			lastErr = err
			return false, nil
		}
		return true, nil
	})
	if pollErr != nil {
		return nil, fmt.Errorf("failed to connect to virtual garden API server %s: %w, last error: %v", restConfig.Host, pollErr, lastErr)
	}

	return &VirtualGardenConfig{
		Client: gardenClient,
		Host:   restConfig.Host,
		CAData: restConfig.CAData,
	}, nil
}

// GetGDCClient initializes and returns the gdcloud TestingClient.
var GetGDCClient = func(ctx context.Context, caData, consoleURL string) (*gdcloud.TestingClient, error) {
	secretData, err := AccessLatestSecret(ctx, "gdc-service-account")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch GDC service account secret: %w", err)
	}
	SAFilePath, err := WriteToFile("secret-*", secretData)
	if err != nil {
		return nil, fmt.Errorf("failed to write secret to file: %w", err)
	}
	defer os.Remove(SAFilePath)

	caDataBytes, err := base64.StdEncoding.DecodeString(caData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode CA data: %w", err)
	}
	CADataPath, err := WriteToFile("cadata-*.pem", caDataBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to write CA data to file: %w", err)
	}

	client, err := gdcloud.NewTestingClient(CADataPath, SAFilePath, consoleURL)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize gdcloud client: %w", err)
	}

	return client, nil
}
