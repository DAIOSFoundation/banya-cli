// `banya eval …` — non-interactive commands that let the
// agent-evaluation harness feed runtime-measured benchmark results
// back into banya's on-disk records file. The harness (Python) shells
// out once per completed smoke run so the record format stays owned
// by this binary.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cascadecodes/banya-cli/internal/domain"
	"github.com/spf13/cobra"
)

func evalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Evaluation helpers (record benchmark results, query coverage)",
	}
	cmd.AddCommand(evalRecordAppendCmd())
	cmd.AddCommand(evalCoverageCmd())
	return cmd
}

func evalRecordAppendCmd() *cobra.Command {
	var (
		domainKey string
		name      string
		sample    int
		passRate  float64
		runID     string
		source    string
		date      string
	)
	cmd := &cobra.Command{
		Use:   "record-append",
		Short: "Append a measured benchmark result to the on-disk records file",
		RunE: func(_ *cobra.Command, _ []string) error {
			if domainKey == "" {
				return fmt.Errorf("--domain is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if sample <= 0 {
				return fmt.Errorf("--sample must be > 0")
			}
			if passRate < 0 || passRate > 1 {
				return fmt.Errorf("--pass-rate must be in [0, 1]")
			}
			rec := domain.Record{
				DomainKey: domainKey,
				Name:      name,
				Sample:    sample,
				PassRate:  passRate,
				Date:      date,
				RunID:     runID,
				Source:    source,
			}
			if err := domain.AppendRecord(rec); err != nil {
				return fmt.Errorf("append record: %w", err)
			}
			fmt.Fprintf(os.Stderr, "[banya-cli] appended record: %s ← %s (%d tasks, %.1f%%)\n",
				domainKey, name, sample, passRate*100)
			return nil
		},
	}
	cmd.Flags().StringVar(&domainKey, "domain", "", "Domain key from coverage.go (e.g. python_scientific_astropy)")
	cmd.Flags().StringVar(&name, "name", "", "Benchmark display name (e.g. 'SWE-bench Lite (stratified)')")
	cmd.Flags().IntVar(&sample, "sample", 0, "Number of tasks measured")
	cmd.Flags().Float64Var(&passRate, "pass-rate", 0, "Pass rate in [0, 1]")
	cmd.Flags().StringVar(&runID, "run-id", "", "Originating run id / tag (optional)")
	cmd.Flags().StringVar(&source, "source", "", "Benchmark family (swebench-lite / webbench-react / humaneval / …)")
	cmd.Flags().StringVar(&date, "date", "", "ISO-8601 date (default: today)")
	return cmd
}

func evalCoverageCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Print the effective domain coverage (static + runtime records)",
		RunE: func(_ *cobra.Command, _ []string) error {
			covs := domain.EffectiveCoverage()
			if jsonOut {
				b, err := json.MarshalIndent(covs, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(b))
				return nil
			}
			for _, d := range covs {
				marker := "·"
				if d.HasValidation() {
					marker = "✓"
				}
				fmt.Printf("%s %-35s %s\n", marker, d.Key, d.Label)
				for _, b := range d.Benchmarks {
					fmt.Printf("    %s  n=%d  pass=%.1f%%  %s\n", b.Date, b.Sample, b.PassRate*100, b.Name)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}
