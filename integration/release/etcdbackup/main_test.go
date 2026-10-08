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

package etcdbackup

import (
	"context"
	"flag"
	"fmt"
	"testing"
	"time"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/core/v1alpha1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/loader"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
)

var (
	gardenerArtifactsVersion        = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath    = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	continuousConfigurationFilePath = flag.String("continuous-configuration-file-path", "", "the path to the continuous configuration file")
	virtualGardenProvider           = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	pollInterval = 30 * time.Second
	pollTimeout  = 5 * time.Minute
)

func TestShootEtcdBackup(t *testing.T) {
	releaseConfigData, err := loader.LoadConfig(loader.LoadConfigOptions{
		ReleaseConfigPath:     *releaseConfigurationFilePath,
		ContinuousConfigPath:  *continuousConfigurationFilePath,
		GardenerVersion:       *gardenerArtifactsVersion,
		VirtualGardenProvider: *virtualGardenProvider,
	})
	if err != nil {
		t.Fatalf("failed to load configuration: %v", err)
	}
	t.Log("Successfully loaded configuration.")

	if releaseConfigData != nil && releaseConfigData.GDCClient != nil {
		defer releaseConfigData.GDCClient.Cleanup()
	}

	ctx := context.Background()

	// Connect to Seed cluster
	var seedWatchClient client.WithWatch
	if releaseConfigData.GDC != nil && releaseConfigData.GDC.UseUserClusterAsSeed {
		if releaseConfigData.RuntimeClusterClient == nil {
			t.Fatal("RuntimeClusterClient is nil on GDC user cluster seed")
		}
		seedWatchClient = releaseConfigData.RuntimeClusterClient
	} else if releaseConfigData.Seed != nil && releaseConfigData.Seed.HostCluster != nil && releaseConfigData.Seed.HostCluster.Shoot != nil {
		remoteShootCluster := releaseConfigData.Seed.HostCluster.Shoot
		shootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
		shootClients, err := gardener.NewShootClient(ctx, remoteShootCluster.VirtualGarden.Client, shootKey)
		if err != nil {
			t.Fatalf("failed to create seed client: %v", err)
		}
		seedWatchClient = shootClients.WatchClient
	} else {
		t.Fatal("no valid Seed cluster configuration found")
	}
	t.Log("Successfully connected to Seed cluster.")

	// Add druid schema to scheme
	s := seedWatchClient.Scheme()
	if err := druidv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add druid schema to scheme: %v", err)
	}
	t.Log("Successfully registered druid schema.")

	// Namespace of the shoot in the seed is shoot--<project>--<name>
	shootNamespace := fmt.Sprintf("shoot--%s--%s", releaseConfigData.TestShoot.Namespace, releaseConfigData.TestShoot.Name)

	t.Run("Verify Shoot ETCD Backup is Enabled", func(t *testing.T) {
		t.Logf("Checking Etcd backup readiness in namespace %s", shootNamespace)
		etcdMainKey := client.ObjectKey{Name: gardenv1beta1constants.ETCDMain, Namespace: shootNamespace}
		err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
			etcd := &druidv1alpha1.Etcd{}
			if err := seedWatchClient.Get(ctx, etcdMainKey, etcd); err != nil {
				if apierrors.IsNotFound(err) {
					t.Fatalf("Etcd resource %q in shoot namespace %q is not found: %v", etcdMainKey.Name, shootNamespace, err)
				}
				t.Logf("Failed to get %q: %v. Retrying...", gardenv1beta1constants.ETCDMain, err)
				return false, nil
			}

			for _, cond := range etcd.Status.Conditions {
				if cond.Type == druidv1alpha1.ConditionTypeBackupReady {
					if cond.Status == druidv1alpha1.ConditionTrue {
						t.Logf("Etcd %q backup is Ready", etcdMainKey.Name)
						return true, nil
					}
					t.Logf("Etcd %q backup condition is %q. Reason: %q, Message: %q", etcdMainKey.Name, cond.Status, cond.Reason, cond.Message)
					return false, nil
				}
			}

			t.Logf("Condition %q not found for Etcd %q", druidv1alpha1.ConditionTypeBackupReady, etcdMainKey.Name)
			return false, nil
		})

		if err != nil {
			t.Fatalf("Timeout waiting for Etcd backup readiness: %v", err)
		}
	})
}
