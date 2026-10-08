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
	"fmt"
	"os"
	"strings"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "status":
		runCheckReleaseJobStatus("status", os.Args[2:])
	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: gardener-release <command> [flags]")
	fmt.Println("\nCommands:")
	fmt.Println("  status           View the status of clusters in the release pipeline")
}

func runCheckReleaseJobStatus(cmdName string, args []string) {
	fs := flag.NewFlagSet(cmdName, flag.ExitOnError)
	configFile := fs.String("config", "/tmp/release_configuration.yaml", "Path to the release configuration file")

	if err := fs.Parse(args); err != nil {
		fmt.Printf("Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Loading configuration...")
	cliCfg, err := release.LoadCLIConfig(context.Background(), *configFile)
	if err != nil {
		fmt.Printf("Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Fetching Cluster Status...")
	activeJobs, availableClusters, err := cliCfg.GetStatus(context.Background())
	if err != nil {
		fmt.Printf("Error fetching cluster status: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nListing Active Release Jobs...")
	printActiveJobsTable(activeJobs)

	fmt.Println("\nListing Available Clusters...")
	printAvailableClustersTable(availableClusters)
}

func printActiveJobsTable(jobs []release.ActiveJobStatus) {
	if len(jobs) == 0 {
		fmt.Println("No active jobs found.")
		return
	}

	headers := []string{"ARTIFACTS VERSION", "RUNTIME CLUSTER", "VIRTUAL GARDEN", "SOIL CLUSTER (SEED HOST)", "CREATED SEEDS", "CREATED SHOOTS"}
	var rows [][]string

	for _, j := range jobs {
		rows = append(rows, []string{
			j.GardenerArtifactsVersion,
			j.RuntimeCluster,
			j.VirtualGarden,
			j.SoilCluster,
			j.CreatedSeed,
			j.CreatedShoot,
		})
	}

	printTable(headers, rows)
}

func printAvailableClustersTable(clusters []release.AvailableClusterStatus) {
	if len(clusters) == 0 {
		fmt.Println("No available clusters found.")
		return
	}

	headers := []string{"NAME", "TYPE", "HOSTED_BY"}
	var rows [][]string

	for _, c := range clusters {
		rows = append(rows, []string{c.Name, c.Type, c.HostedBy})
	}

	printTable(headers, rows)
}

func printTable(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	// Calculate widths
	for _, row := range rows {
		for i, cell := range row {
			lines := strings.Split(cell, "\n")
			for _, line := range lines {
				l := visibleLen(line)
				if l > widths[i] {
					widths[i] = l
				}
			}
		}
	}

	printSeparator := func() {
		for _, w := range widths {
			fmt.Print("+")
			for i := 0; i < w+2; i++ { // +2 for padding spaces
				fmt.Print("-")
			}
		}
		fmt.Println("+")
	}

	printRowLine := func(cells []string) {
		for i, cell := range cells {
			fmt.Print("| ")
			fmt.Print(cell)
			padding := widths[i] - visibleLen(cell)
			for k := 0; k < padding; k++ {
				fmt.Print(" ")
			}
			fmt.Print(" ")
		}
		fmt.Println("|")
	}

	printSeparator()
	// Print headers
	printRowLine(headers)
	printSeparator()

	// Print rows
	for _, row := range rows {
		maxHeight := 0
		// Split all cells in this row
		var rowLines [][]string
		for _, cell := range row {
			lines := strings.Split(cell, "\n")
			if len(lines) > maxHeight {
				maxHeight = len(lines)
			}
			rowLines = append(rowLines, lines)
		}

		for i := 0; i < maxHeight; i++ {
			var lineCells []string
			for _, cellLines := range rowLines {
				line := ""
				if i < len(cellLines) {
					line = cellLines[i]
				}
				lineCells = append(lineCells, line)
			}
			printRowLine(lineCells)
		}
		printSeparator()
	}
}

func visibleLen(s string) int {
	l := 0
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		l++
	}
	return l
}
