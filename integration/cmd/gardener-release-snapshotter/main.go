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

package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	config "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/snapshot"
)

var (
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "Path to the release configuration YAML file (required)")
	snapshotConfigPath           = flag.String("snapshot-config-path", "", "Path to the snapshot resources YAML file (optional, defaults to capturing all resources)")
	gardenerArtifactsVersion     = flag.String("gardener-artifacts-version", "", "Gardener artifacts version for loading release config (required)")
	bucket                       = flag.String("bucket", "gardener-ci-pipeline", "GCS bucket for uploading snapshots")
	destDir                      = flag.String("dest-dir", "", "GCS destination directory prefix (required)")
	virtualGardenProvider        = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	ClusterShoot         = "shoot-cluster"
	ClusterVirtualGarden = "garden-cluster"
	ClusterSeed          = "seed-cluster"
	ClusterRuntime       = "runtime-cluster"
	ClusterGlobal        = "global-api-cluster"
)

var supportedClusters = []string{ClusterShoot, ClusterVirtualGarden, ClusterSeed, ClusterRuntime, ClusterGlobal}

func main() {
	flag.Parse()

	if *releaseConfigurationFilePath == "" || *gardenerArtifactsVersion == "" || *destDir == "" {
		flag.Usage()
		os.Exit(1)
	}

	ctx := context.Background()

	log.Printf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releasePipelineCfg, err := config.LoadReleaseTestConfig(
		*releaseConfigurationFilePath,
		*gardenerArtifactsVersion,
		config.WithVirtualGardenProvider(*virtualGardenProvider),
	)
	if err != nil {
		log.Fatalf("Failed to load release configuration: %v", err)
	}

	if releasePipelineCfg != nil && releasePipelineCfg.GDCClient != nil {
		defer releasePipelineCfg.GDCClient.Cleanup()
	}

	var resources *snapshot.MultiClusterSnapshotResources
	if *snapshotConfigPath != "" {
		log.Printf("Loading snapshot resources from %s", *snapshotConfigPath)
		var err error
		resources, err = snapshot.LoadSnapshotResources(*snapshotConfigPath)
		if err != nil {
			log.Fatalf("Failed to load snapshot resources: %v", err)
		}
	} else {
		log.Println("snapshot-config-path not specified, will automatically discover and snapshot all resources")
	}

	uploadConfig := snapshot.DestinationConfig{
		Bucket:  *bucket,
		DestDir: *destDir,
	}

	combinedSnapshotsBuffer, err := os.MkdirTemp("", "snapshot-buffer-*")
	if err != nil {
		log.Fatalf("Failed to create temporary buffer directory: %v", err)
	}
	defer os.RemoveAll(combinedSnapshotsBuffer)

	for _, clusterName := range supportedClusters {
		// Find config for this cluster in snapshot config
		var clusterSnapshotConfig *snapshot.ClusterSnapshotConfig
		if resources != nil {
			for i := range resources.Clusters {
				if resources.Clusters[i].ClusterName == clusterName {
					clusterSnapshotConfig = &resources.Clusters[i]
					break
				}
			}
		}

		var crClient client.Client
		var clientset kubernetes.Interface
		var isActive bool

		hasVirtualGardenClient := releasePipelineCfg.TestShoot != nil &&
			releasePipelineCfg.TestShoot.VirtualGarden != nil &&
			releasePipelineCfg.TestShoot.VirtualGarden.Client != nil

		switch clusterName {
		case ClusterShoot:
			isActive = hasVirtualGardenClient &&
				releasePipelineCfg.TestShoot.Namespace != "" &&
				releasePipelineCfg.TestShoot.Name != ""
			if isActive {
				shootKey := client.ObjectKey{
					Namespace: releasePipelineCfg.TestShoot.Namespace,
					Name:      releasePipelineCfg.TestShoot.Name,
				}
				log.Printf("Creating client for Shoot %q in namespace %q", shootKey.Name, shootKey.Namespace)
				shootClients, err := gardener.NewShootClient(ctx, releasePipelineCfg.TestShoot.VirtualGarden.Client, shootKey)
				if err != nil {
					log.Printf("Warning: Failed to create client for Shoot %s: %v. Skipping snapshot.", shootKey.Name, err)
					continue
				}
				crClient = shootClients.WatchClient
				clientset = shootClients.Client
			}

		case ClusterVirtualGarden:
			isActive = hasVirtualGardenClient
			if isActive {
				crClient = releasePipelineCfg.TestShoot.VirtualGarden.Client
			}

		case ClusterSeed:
			isActive = releasePipelineCfg.Seed != nil &&
				releasePipelineCfg.Seed.HostCluster != nil &&
				releasePipelineCfg.Seed.HostCluster.Shoot != nil &&
				releasePipelineCfg.Seed.HostCluster.Shoot.VirtualGarden != nil
			if isActive {
				hostShoot := releasePipelineCfg.Seed.HostCluster.Shoot
				hostShootKey := client.ObjectKey{
					Namespace: hostShoot.Namespace,
					Name:      hostShoot.Name,
				}
				log.Printf("Creating client for seed-hosting Shoot %q in namespace %q", hostShootKey.Name, hostShootKey.Namespace)
				seedClients, err := gardener.NewShootClient(ctx, releasePipelineCfg.Seed.HostCluster.Shoot.VirtualGarden.Client, hostShootKey)
				if err != nil {
					log.Printf("Warning: Failed to create client for seed-hosting Shoot %s: %v. Skipping snapshot.", hostShootKey.Name, err)
					continue
				}
				crClient = seedClients.WatchClient
				clientset = seedClients.Client
			}

		case ClusterRuntime:
			isActive = releasePipelineCfg.RuntimeClusterClient != nil
			if isActive {
				crClient = releasePipelineCfg.RuntimeClusterClient
			}

		case ClusterGlobal:
			isActive = releasePipelineCfg.GlobalAPIClient != nil
			if isActive {
				crClient = releasePipelineCfg.GlobalAPIClient
			}
			if clusterSnapshotConfig == nil {
				// For the GDC global API, only snapshot the project and secondary project namespaces.
				// The service account only has permission in these projects.
				clusterSnapshotConfig = &snapshot.ClusterSnapshotConfig{
					ClusterName: ClusterGlobal,
					Targets: []snapshot.NamespaceTarget{
						{Namespace: releasePipelineCfg.GDC.Project},
						{Namespace: releasePipelineCfg.GDC.SecondaryProject},
					},
				}
			}
		}

		if !isActive {
			log.Printf("Cluster %s is not active in release configuration, skipping.", clusterName)
			continue
		}

		if crClient == nil {
			log.Printf("Warning: Client for cluster %s is nil despite being active and configured. Skipping.", clusterName)
			continue
		}

		log.Printf("Starting snapshot capture for cluster %s", clusterName)
		clusterDir := filepath.Join(combinedSnapshotsBuffer, clusterName)
		err = snapshot.CaptureSnapshotToDir(ctx, snapshot.SnapshotClients{CRClient: crClient, ClientSet: clientset}, clusterDir, snapshot.CaptureOptions{Config: clusterSnapshotConfig})
		if err != nil {
			log.Printf("Warning: Snapshot capture failed for cluster %s: %v", clusterName, err)
		}
	}

	log.Println("Uploading combined snapshots archive to gs://gardener-ci-pipeline")
	if err := snapshot.UploadArchive(ctx, combinedSnapshotsBuffer, &uploadConfig, "all-clusters"); err != nil {
		log.Fatalf("Failed to upload snapshot archive: %v", err)
	}

	log.Println("Snapshot completed successfully")
}
