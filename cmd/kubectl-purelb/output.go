// Copyright 2026 Acnodal Inc.
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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// outputFormat holds the user's requested output format.
type outputFormat string

const (
	outputTable outputFormat = ""
	outputJSON  outputFormat = "json"
	outputYAML  outputFormat = "yaml"
)

// parseOutputFormat validates the -o flag value.
func parseOutputFormat(s string) (outputFormat, error) {
	switch strings.ToLower(s) {
	case "", "table":
		return outputTable, nil
	case "json":
		return outputJSON, nil
	case "yaml":
		return outputYAML, nil
	default:
		return "", fmt.Errorf("unknown output format %q (valid: table, json, yaml)", s)
	}
}

// printStructured outputs data as JSON or YAML to stdout.
func printStructured(format outputFormat, data interface{}) error {
	switch format {
	case outputJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case outputYAML:
		b, err := yaml.Marshal(data)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	default:
		return fmt.Errorf("printStructured called with table format")
	}
}

// tableWriter returns a tabwriter configured for aligned column output.
func tableWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}
