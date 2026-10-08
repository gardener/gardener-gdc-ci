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

package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/gcs"
)

// uploadFile allows overriding the GCS upload function for hermetic unit testing.
var uploadFile = gcs.UploadFile

const defaultDirPermissions = 0755

// DestinationConfig defines the destination bucket and directory prefix for uploading generated snapshots.
type DestinationConfig struct {
	// Bucket is the Google Cloud Storage bucket name where the resulting snapshot archives are stored.
	Bucket string
	// DestDir specifies the path prefix inside the bucket under which the snapshots will be uploaded.
	DestDir string
}

type SnapshotClients struct {
	// CRClient is the controller-runtime client for regular resource operations.
	CRClient client.Client
	// ClientSet is the standard kubernetes clientset for operations like fetching logs.
	ClientSet kubernetes.Interface
}

// CaptureOptions defines optional parameters for capturing snapshots.
type CaptureOptions struct {
	// Config defines the resources to capture. If nil, all known resources are discovered and captured.
	Config *ClusterSnapshotConfig
}

// CaptureSnapshotToDir retrieves the specified resources dynamically and saves the resulting manifests into the output directory.
func CaptureSnapshotToDir(ctx context.Context, clients SnapshotClients, dir string, opts CaptureOptions) error {
	config := opts.Config
	var errs []error

	if config == nil {
		log.Println("clusterSnapshotConfig is nil or not provided, automatically discovering all known API versions and resources")
		config = &ClusterSnapshotConfig{
			ClusterName: filepath.Base(dir),
			Targets: []NamespaceTarget{
				{
					Resources: discoverKnownAPIs(clients.CRClient),
				},
			},
		}
	}

	if config.ClusterName == "" {
		return fmt.Errorf("cluster name must be specified in config")
	}

	for _, target := range config.Targets {
		namespaces, err := getNamespaces(ctx, clients.CRClient, target)
		if err != nil {
			return err
		}

		if target.Resources == nil {
			log.Printf("target.Resources is nil for target (namespace: %q, matchLabels: %v), automatically discovering all known APIs", target.Namespace, target.MatchLabels)
			target.Resources = discoverKnownAPIs(clients.CRClient)
		}

		for _, res := range target.Resources {
			gv, err := schema.ParseGroupVersion(res.APIVersion)
			if err != nil {
				return fmt.Errorf("failed to parse apiVersion %s: %w", res.APIVersion, err)
			}
			mapping, err := clients.CRClient.RESTMapper().RESTMapping(schema.GroupKind{Group: gv.Group, Kind: res.Kind}, gv.Version)
			if err != nil {
				return fmt.Errorf("failed to find REST mapping for %s %s: %w", res.APIVersion, res.Kind, err)
			}
			resourceName := mapping.Resource.Resource

			gvk := schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    res.Kind + "List",
			}

			isNamespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
			targetNamespaces := namespaces
			if !isNamespaced {
				targetNamespaces = []string{"_cluster_scoped_"}
			}

			for _, ns := range targetNamespaces {
				log.Printf("Capturing resource %s/%s (Version: %s, Kind: %s)", ns, resourceName, gv.Version, res.Kind)
				// Structure: <namespace>/<resource-name>
				resDir := filepath.Join(dir, ns, resourceName)
				if err := os.MkdirAll(resDir, defaultDirPermissions); err != nil {
					return fmt.Errorf("failed to create directory %s: %w", resDir, err)
				}

				// Create a new UnstructuredList for each resource type.
				uList := &unstructured.UnstructuredList{}
				uList.SetGroupVersionKind(gvk)

				listOpts := []client.ListOption{}
				if isNamespaced {
					listOpts = append(listOpts, client.InNamespace(ns))
				}

				if err := clients.CRClient.List(ctx, uList, listOpts...); err != nil {
					errs = append(errs, fmt.Errorf("failed to list resource %s in %s: %w", resourceName, ns, err))
					continue
				}

				log.Printf("Found %d items for resource target %s/%s", len(uList.Items), ns, resourceName)
				for _, item := range uList.Items {
					filePath := filepath.Join(resDir, item.GetName()+".yaml")
					if err := saveYAML(filePath, item.Object); err != nil {
						return err
					}

					// Fetch logs if the resource is a Pod and ClientSet is available.
					if strings.EqualFold(res.Kind, "Pod") {
						if clients.ClientSet != nil {
							pod := &corev1.Pod{}
							err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, pod)
							if err != nil {
								return fmt.Errorf("failed to convert unstructured to pod: %w", err)
							}
							if err := capturePodLogs(ctx, clients.ClientSet, ns, pod, resDir); err != nil {
								log.Printf("Warning: Failed to capture logs for pod %s in ns %s in cluster %s: %v", pod.Name, ns, config.ClusterName, err)
							}
						} else {
							log.Printf("Warning: ClientSet is nil for cluster %s, skipping log capture for pod %s in namespace %s", config.ClusterName, item.GetName(), ns)
						}
					}
				}
			}
		}
	}
	if len(errs) > 0 {
		log.Printf("Capture finished with %d errors:", len(errs))
		for _, err := range errs {
			log.Printf("  - %v", err)
		}
		return &PartialSnapshotError{Errors: errs}
	}
	return nil
}

func capturePodLogs(ctx context.Context, clientset kubernetes.Interface, ns string, pod *corev1.Pod, resDir string) error {
	var containerNames []string
	for _, c := range pod.Spec.InitContainers {
		containerNames = append(containerNames, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		containerNames = append(containerNames, c.Name)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		containerNames = append(containerNames, c.Name)
	}

	for _, containerName := range containerNames {
		err := func() (err error) {
			podLogs, err := clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{Container: containerName}).Stream(ctx)
			if err != nil {
				log.Printf("Warning: Failed to fetch logs for pod %s container %s in ns %s: %v", pod.Name, containerName, ns, err)
				return nil // continue
			}
			defer podLogs.Close()

			logFilePath := filepath.Join(resDir, fmt.Sprintf("%s-%s.log", pod.Name, containerName))
			logFile, err := os.Create(logFilePath)
			if err != nil {
				return fmt.Errorf("failed to create log file %s: %w", logFilePath, err)
			}
			defer func() {
				if cerr := logFile.Close(); cerr != nil && err == nil {
					err = fmt.Errorf("failed to close log file %s: %w", logFilePath, cerr)
				}
			}()

			_, err = io.Copy(logFile, podLogs)
			if err != nil {
				return fmt.Errorf("failed to write logs to %s: %w", logFilePath, err)
			}

			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// UploadArchive bundles the contents of srcDir into a tar.gz archive and uploads it to GCS.
func UploadArchive(ctx context.Context, srcDir string, upload *DestinationConfig, prefix string) error {
	if upload.Bucket == "" {
		return fmt.Errorf("upload bucket must be specified")
	}

	archivePath := filepath.Join(srcDir, "snapshots-archive.tar.gz")
	log.Printf("Creating tar archive at %s", archivePath)
	if err := createArchive(srcDir, archivePath); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	options := gcs.UploadOptions{
		BucketName:    upload.Bucket,
		ObjectName:    filepath.Join(upload.DestDir, snapshotArchiveName(prefix, timestamp)),
		LocalFilePath: archivePath,
	}
	log.Printf("Uploading snapshot archive to GCS bucket %s (Object: %s)", options.BucketName, options.ObjectName)
	if err := uploadFile(ctx, options); err != nil {
		return fmt.Errorf("failed to upload snapshot archive to GCS: %w", err)
	}

	return nil
}

func saveYAML(path string, obj interface{}) error {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("failed to marshal object: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// createArchive bundles the contents of srcDir into a tar.gz archive at archivePath.
// It is a package-level variable to allow mocking in tests.
var createArchive = func(srcDir, archivePath string) error {
	// Create the destination output file for the archive.
	destFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	// Initialize the gzip writer wrapping the output file.
	zipWriter := gzip.NewWriter(destFile)
	defer zipWriter.Close()

	// Initialize the tar writer wrapping the gzip writer.
	tarWriter := tar.NewWriter(zipWriter)
	defer tarWriter.Close()

	// Walk through the source directory recursively to process files.
	err = filepath.WalkDir(srcDir, func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Prevent self-inclusion if the destination file is within the source directory.
		if path == archivePath {
			return nil
		}

		// Calculate relative path to maintain directory structure inside the archive.
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		// Skip the root directory itself.
		if relPath == "." {
			return nil
		}

		info, err := dirEntry.Info()
		if err != nil {
			return fmt.Errorf("failed to get file info for %s: %w", path, err)
		}

		// Construct the tar header based on file info.
		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return fmt.Errorf("failed to create tar header for %s: %w", path, err)
		}

		// Set the correct relative name inside the archive.
		header.Name = filepath.ToSlash(relPath)

		// Write the header definition to the archive stream.
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", path, err)
		}

		// directories do not require payload copies.
		if !info.Mode().IsRegular() {
			return nil
		}

		// Wrap file opening and copying in an anonymous function to ensure
		// file descriptors are closed immediately via defer, preventing leaks.
		if err := func() error {
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("failed to open file %s: %w", path, err)
			}
			defer file.Close()

			if _, err := io.Copy(tarWriter, file); err != nil {
				return fmt.Errorf("failed to copy file %s to tar: %w", path, err)
			}
			return nil
		}(); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return err
	}

	// Ensure all buffers finalize cleanly.
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := zipWriter.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}

	// Explicitly close destFile and check for errors.
	if err := destFile.Close(); err != nil {
		return fmt.Errorf("failed to close destination file: %w", err)
	}

	return nil
}

func snapshotArchiveName(prefix, timestamp string) string {
	if prefix != "" {
		return fmt.Sprintf("snapshots-%s-%s.tar.gz", prefix, timestamp)
	}
	return fmt.Sprintf("snapshots-%s.tar.gz", timestamp)
}

func getNamespaces(ctx context.Context, k8sClient client.Client, target NamespaceTarget) ([]string, error) {
	hasLabels := len(target.MatchLabels) > 0
	if target.Namespace != "" && hasLabels {
		return nil, fmt.Errorf("cannot specify both namespace and matchLabels in target")
	}
	if target.Namespace != "" {
		return []string{target.Namespace}, nil
	}

	uList := &unstructured.UnstructuredList{}
	uList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "NamespaceList",
	})

	listOpts := []client.ListOption{}
	if hasLabels {
		listOpts = append(listOpts, client.MatchingLabels(target.MatchLabels))
	}

	if err := k8sClient.List(ctx, uList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}

	var namespaces []string
	for _, item := range uList.Items {
		namespaces = append(namespaces, item.GetName())
	}

	if len(namespaces) == 0 {
		if hasLabels {
			return nil, fmt.Errorf("no namespaces found with labels %v", target.MatchLabels)
		}
		return nil, fmt.Errorf("no namespaces found in cluster")
	}

	if hasLabels {
		log.Printf("Found %d namespaces with labels %v: %v", len(namespaces), target.MatchLabels, namespaces)
	} else {
		log.Printf("Namespace target is not specified, found %d namespaces across cluster", len(namespaces))
	}
	return namespaces, nil
}

// discoverKnownAPIs traverses the client's Scheme and RESTMapper to dynamically identify
// all available APIs installed on this client, returning them as ResourceTargets.
func discoverKnownAPIs(crClient client.Client) []ResourceTarget {
	var discoveredResources []ResourceTarget
	if crClient == nil {
		return discoveredResources
	}

	seen := make(map[string]bool)
	for gvk := range crClient.Scheme().AllKnownTypes() {
		if gvk.Version == "" || gvk.Version == "__internal" || strings.HasSuffix(gvk.Kind, "List") {
			continue
		}

		_, err := crClient.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			continue
		}

		resTarget := ResourceTarget{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
		}
		key := resTarget.APIVersion + "/" + resTarget.Kind
		if !seen[key] {
			seen[key] = true
			discoveredResources = append(discoveredResources, resTarget)
		}
	}
	return discoveredResources
}
