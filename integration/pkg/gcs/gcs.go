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

package gcs

import (
	"context"
	"fmt"
	"io"
	"os"

	"cloud.google.com/go/storage"
)

// UploadOptions specifies custom parameters for uploading files to Google Cloud Storage.
type UploadOptions struct {
	BucketName    string
	ObjectName    string
	LocalFilePath string
	Metadata      map[string]string
}

// UploadFile securely pushes the designated local file to the specified GCS bucket path while attaching custom metadata.
func UploadFile(ctx context.Context, options UploadOptions) error {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create GCS client: %w", err)
	}
	defer client.Close()

	f, err := os.Open(options.LocalFilePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", options.LocalFilePath, err)
	}
	defer f.Close()

	obj := client.Bucket(options.BucketName).Object(options.ObjectName)
	wc := obj.NewWriter(ctx)

	if options.Metadata != nil {
		wc.ObjectAttrs.Metadata = options.Metadata
	}

	if _, err := io.Copy(wc, f); err != nil {
		wc.Close()
		return fmt.Errorf("failed to stream local file to GCS object: %w", err)
	}

	if err := wc.Close(); err != nil {
		return fmt.Errorf("failed to finalize GCS object writer: %w", err)
	}

	return nil
}
