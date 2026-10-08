// Copyright 2025 Google LLC
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

package sonobuoy

import (
	"fmt"
	"os"
	"os/exec"
)

var sonobuoyLocation = func() string {
	if loc := os.Getenv("SONOBUOY_PATH"); loc != "" {
		return loc
	}
	return "sonobuoy"
}

// Exec executes the sonobuoy binary with the given arguments.
func Exec(args ...string) ([]byte, error) {
	loc := sonobuoyLocation()
	if loc == "" {
		return nil, fmt.Errorf("cannot locate sonobuoy binary")
	}
	executable, err := exec.LookPath(loc)
	if err != nil {
		return nil, fmt.Errorf("cannot locate sonobuoy binary: %w", err)
	}
	cmd := exec.Command(executable, args...)
	result, err := cmd.CombinedOutput()
	if err != nil {
		return result, fmt.Errorf("cannot execute sonobuoy command: %w", err)
	}
	return result, nil
}
