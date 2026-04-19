// Runtime-measured benchmark records stored in a user file, not baked
// into the binary. Purpose: every smoke / evaluation run can
// auto-append its measured pass rate so future Scan() calls reflect
// the latest coverage without requiring a rebuild.
//
// Two-layer merge model:
//
//   ┌────────────────┐    ┌────────────────┐
//   │ coverage.go    │    │ records.yaml   │
//   │ (immutable,    │    │ (mutable,      │
//   │  shipped with  │ ⇒  │  runtime-only, │
//   │  the binary)   │    │  auto-updated) │
//   └────────┬───────┘    └────────┬───────┘
//            └───── merged by ─────┘
//                   EffectiveCoverage()
//
// Records.yaml lives at $BANYA_BENCHMARK_RECORDS (if set), else at
// $XDG_CONFIG_HOME/banya/benchmark_records.yaml, else at
// ~/.config/banya/benchmark_records.yaml. Append-only for audit — old
// entries stay; newer entries for the same (domain, benchmark) pair
// win by date when BestPassRate() is computed.

package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record mirrors Benchmark but carries the source run_id / sample_size
// audit fields and an explicit domain key so Append / Merge can route
// the record to the right Domain at merge time.
type Record struct {
	DomainKey string  `yaml:"domain_key"`
	Name      string  `yaml:"name"`
	Sample    int     `yaml:"sample"`
	PassRate  float64 `yaml:"pass_rate"`
	Date      string  `yaml:"date"`               // ISO-8601
	RunID     string  `yaml:"run_id,omitempty"`   // tag from agent-eval
	Source    string  `yaml:"source,omitempty"`   // "swebench-lite" | "webbench-react" | …
}

// recordsMu guards the on-disk file against concurrent append from a
// multi-task harness spawn. Per-run-id appends are naturally serial
// but we lock anyway — the file is tiny.
var recordsMu sync.Mutex

// RecordsPath resolves the on-disk location. Callers rarely need this
// directly; LoadRecords / AppendRecord handle it internally.
func RecordsPath() string {
	if p := os.Getenv("BANYA_BENCHMARK_RECORDS"); p != "" {
		return p
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "banya", "benchmark_records.yaml")
}

// LoadRecords reads and parses the on-disk record file. Missing file
// returns (nil, nil) — that's the normal "first run ever" state.
// Parse errors return ([], nil) rather than propagating; observability
// data should never crash a live task.
func LoadRecords() []Record {
	path := RecordsPath()
	if path == "" {
		return nil
	}
	recordsMu.Lock()
	defer recordsMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseRecordsYAML(string(data))
}

// AppendRecord appends a single record to the on-disk file, creating
// the file + parent dir on first use. The record is stamped with
// `Date` = today (UTC) when the caller left it blank.
func AppendRecord(r Record) error {
	if r.DomainKey == "" {
		return fmt.Errorf("AppendRecord: empty DomainKey")
	}
	if r.Date == "" {
		r.Date = time.Now().UTC().Format("2006-01-02")
	}
	path := RecordsPath()
	if path == "" {
		return fmt.Errorf("AppendRecord: could not resolve records path (set BANYA_BENCHMARK_RECORDS)")
	}
	recordsMu.Lock()
	defer recordsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f,
		"- domain_key: %s\n  name: %q\n  sample: %d\n  pass_rate: %.4f\n  date: %s\n  run_id: %q\n  source: %q\n",
		r.DomainKey, r.Name, r.Sample, r.PassRate, r.Date, r.RunID, r.Source,
	)
	return err
}

// EffectiveCoverage returns the static coverage list enriched with
// every matching Record from the on-disk file. Records with a unknown
// DomainKey are silently ignored (schemas diverge naturally as the
// coverage list grows). The result is a fresh slice — callers can mutate
// freely.
func EffectiveCoverage() []Domain {
	base := All()
	recs := LoadRecords()
	if len(recs) == 0 {
		return base
	}
	byKey := map[string]int{}
	for i, d := range base {
		byKey[d.Key] = i
	}
	for _, r := range recs {
		idx, ok := byKey[r.DomainKey]
		if !ok {
			continue
		}
		base[idx].Benchmarks = append(base[idx].Benchmarks, Benchmark{
			Name:     r.Name,
			Sample:   r.Sample,
			PassRate: r.PassRate,
			Date:     r.Date,
		})
	}
	// Sort each domain's Benchmarks by date desc so BestPassRate sees
	// the freshest sample-weighted first.
	for i := range base {
		bs := base[i].Benchmarks
		sort.SliceStable(bs, func(a, b int) bool { return bs[a].Date > bs[b].Date })
	}
	return base
}

// parseRecordsYAML is a tiny hand-rolled reader for the append-friendly
// format AppendRecord writes. Not a general YAML parser — only accepts
// the exact shape we emit. Keeps the Go side dependency-free; a full
// YAML lib pulls ~300 KB into the binary.
func parseRecordsYAML(text string) []Record {
	var out []Record
	var cur Record
	inItem := false
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, " \t\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "- ") {
			if inItem {
				if cur.DomainKey != "" {
					out = append(out, cur)
				}
			}
			cur = Record{}
			inItem = true
			line = "  " + strings.TrimPrefix(line, "- ")
		}
		trim := strings.TrimLeft(line, " ")
		colon := strings.Index(trim, ":")
		if colon < 0 {
			continue
		}
		key := trim[:colon]
		val := strings.TrimSpace(trim[colon+1:])
		val = strings.Trim(val, `"`)
		switch key {
		case "domain_key":
			cur.DomainKey = val
		case "name":
			cur.Name = val
		case "sample":
			fmt.Sscanf(val, "%d", &cur.Sample)
		case "pass_rate":
			fmt.Sscanf(val, "%f", &cur.PassRate)
		case "date":
			cur.Date = val
		case "run_id":
			cur.RunID = val
		case "source":
			cur.Source = val
		}
	}
	if inItem && cur.DomainKey != "" {
		out = append(out, cur)
	}
	return out
}
