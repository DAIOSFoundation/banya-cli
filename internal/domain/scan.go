// Deterministic workspace → domain classification.
//
// Scan walks the workspace root (bounded depth, bounded file count) and
// builds a cheap signature — extension histogram, marker-file set,
// keyword hit set from dependency manifests. Each Domain in
// coverage.go scores itself against the signature; the highest score
// is the "match", along with a confidence tier.
//
// No regex, no tree-sitter. A workspace full of .py + django in
// requirements.txt should classify in < 50ms on a cold FS.

package domain

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanResult is the full classification output. Top exposes the single
// best match so callers (run.go) can branch on it without walking the
// whole ranked list.
type ScanResult struct {
	// All domains scored, highest first. Zero-score domains are dropped.
	Ranked []Ranked
	// Convenience pointer into Ranked[0]; zero when nothing matched.
	Top *Ranked
	// Top score reported as a confidence tier — "high" / "medium" /
	// "low" / "none" — after normalisation. See tierFor for thresholds.
	Tier string
	// Raw signature we computed (exposed so tests can assert).
	Sig Signature
}

// Ranked pairs a domain with its normalised score (0..1).
type Ranked struct {
	Domain Domain
	Score  float64 // 0..1, 1 = perfect match
}

// Signature is the static fingerprint extracted from a workspace.
type Signature struct {
	ExtCounts    map[string]int  // ".py" → 42, ".tsx" → 5, …
	Markers      map[string]bool // set of basenames found anywhere in scope
	Keywords     map[string]bool // lowercased keyword hits from manifests
	ManifestText string          // concatenated manifest bodies (trimmed)
	TotalFiles   int             // number of scanned files (for density)
}

const (
	// File-count cap for a single workspace scan. Beyond this we stop
	// descending — avoids pathological node_modules / venv traversals
	// from hijacking the whole classifier.
	maxScanFiles = 4000
	// Depth cap (relative to root).
	maxScanDepth = 8
)

// ignoredDirs are skipped during the walk. Large vendor trees only
// dilute the signal.
var ignoredDirs = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"venv":         true,
	".venv":        true,
	"env":          true,
	"__pycache__":  true,
	".mypy_cache":  true,
	".pytest_cache": true,
	"dist":         true,
	"build":        true,
	"target":       true, // Rust build output — we still match via Cargo.toml at root
	".next":        true,
	".nuxt":        true,
	".cache":       true,
	"coverage":     true,
}

// manifestBasenames are files whose text we slurp into Signature.ManifestText
// for keyword scanning. Keep the list small — reading whole source trees
// blows the time budget.
var manifestBasenames = []string{
	"requirements.txt", "requirements-dev.txt", "pyproject.toml", "setup.py",
	"setup.cfg", "Pipfile", "poetry.lock",
	"package.json", "pnpm-lock.yaml", "yarn.lock",
	"Cargo.toml", "Cargo.lock",
	"go.mod",
	"Gemfile",
	"Podfile", "Package.swift",
	"build.gradle", "build.gradle.kts", "settings.gradle.kts",
}

// Scan classifies a workspace. root is usually the user's repo root or
// the working directory for a banya-cli invocation.
func Scan(root string) ScanResult {
	sig := Signature{
		ExtCounts: map[string]int{},
		Markers:   map[string]bool{},
		Keywords:  map[string]bool{},
	}
	if root == "" {
		return ScanResult{Sig: sig, Tier: "none"}
	}

	// Collect the extension/marker signature via a bounded walk.
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // keep walking; permission errors are fine
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator))
		if depth > maxScanDepth {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if ignoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		sig.TotalFiles++
		if sig.TotalFiles > maxScanFiles {
			return filepath.SkipAll
		}
		name := d.Name()
		sig.Markers[name] = true // basename seen anywhere counts as a marker
		ext := strings.ToLower(filepath.Ext(name))
		if ext != "" {
			sig.ExtCounts[ext]++
		}
		return nil
	})

	// Slurp manifest files to feed the keyword hit list. Whole-tree
	// keyword scans would be too noisy — manifests are the canonical
	// declaration surface.
	var manifestBuf strings.Builder
	for _, mf := range manifestBasenames {
		// Look for manifest at root first (most common), then nested.
		p := filepath.Join(root, mf)
		if data, err := os.ReadFile(p); err == nil {
			manifestBuf.Write(data)
			manifestBuf.WriteByte('\n')
		}
	}
	sig.ManifestText = strings.ToLower(manifestBuf.String())
	// Source-of-truth for Scan: effective coverage = static list +
	// runtime-measured records. This keeps Scan() self-contained so
	// callers don't need to remember the merge step.
	effective := EffectiveCoverage()
	for _, dom := range effective {
		for _, kw := range dom.Keywords {
			if kw == "" {
				continue
			}
			if strings.Contains(sig.ManifestText, strings.ToLower(kw)) {
				sig.Keywords[strings.ToLower(kw)] = true
			}
		}
	}

	// Score every domain.
	var ranked []Ranked
	for _, dom := range effective {
		score := scoreDomain(dom, sig)
		if score > 0 {
			ranked = append(ranked, Ranked{Domain: dom, Score: score})
		}
	}
	// Descending sort without importing sort (keeps this file tight —
	// the list is ≤ len(coverage) ≈ 12 entries).
	for i := range ranked {
		for j := i + 1; j < len(ranked); j++ {
			if ranked[j].Score > ranked[i].Score {
				ranked[i], ranked[j] = ranked[j], ranked[i]
			}
		}
	}

	res := ScanResult{Ranked: ranked, Sig: sig}
	if len(ranked) > 0 {
		res.Top = &ranked[0]
		res.Tier = tierFor(
			ranked[0].Score,
			ranked[0].Domain.HasValidation(),
			ranked[0].Domain.TotalSample(),
		)
	} else {
		res.Tier = "none"
	}
	return res
}

// scoreDomain returns a number in [0, 1]. Three ingredients:
//
//   - extension share: % of scanned files whose extension belongs to the
//     domain. Caps at 1.0 when every file matches.
//   - marker hits: each marker basename counts for a fixed boost.
//   - keyword hits: each matched keyword adds a smaller boost.
//
// The weights were chosen so a workspace with mostly .py + django in
// requirements.txt lands at ~0.9 for python_web. Tuning past that point
// should be data-driven — pin a dataset and tweak.
func scoreDomain(dom Domain, sig Signature) float64 {
	var extHit int
	domExts := map[string]bool{}
	for _, e := range dom.Extensions {
		domExts[normalizeExt(e)] = true
	}
	for ext, n := range sig.ExtCounts {
		if domExts[ext] {
			extHit += n
		}
	}
	var extShare float64
	if sig.TotalFiles > 0 {
		extShare = float64(extHit) / float64(sig.TotalFiles)
		if extShare > 1 {
			extShare = 1
		}
	}

	var markerHit int
	for _, m := range dom.Markers {
		if sig.Markers[m] {
			markerHit++
		}
	}
	markerScore := 0.15 * float64(markerHit)
	if markerScore > 0.45 {
		markerScore = 0.45
	}

	var kwHit int
	for _, kw := range dom.Keywords {
		if sig.Keywords[strings.ToLower(kw)] {
			kwHit++
		}
	}
	kwScore := 0.1 * float64(kwHit)
	if kwScore > 0.4 {
		kwScore = 0.4
	}

	// Combine. extShare 0..1 accounts for the dominant signal; markers/
	// keywords add confidence when the file mix is ambiguous.
	s := 0.5*extShare + markerScore + kwScore
	if s > 1 {
		s = 1
	}
	return s
}

// tierFor maps a raw score + validation status + sample size to a
// user-visible tier.
//
//   - "high":   score ≥ 0.6 AND domain validated AND total sample ≥
//     MinSampleForHighTier (5). A single-task benchmark doesn't earn
//     high confidence — its Wilson CI is too wide.
//   - "medium": score ≥ 0.6 but no benchmark / low sample, OR
//     score in [0.35, 0.6) with any validation signal.
//   - "low":    score in [0.15, 0.35).
//   - "none":   below 0.15 — treat as out-of-distribution.
//
// `sample` is the TotalSample() across all recorded benchmarks for the
// top-matched domain. Pass 0 when the caller doesn't have that signal.
func tierFor(score float64, validated bool, sample int) string {
	highQualityValidated := validated && sample >= MinSampleForHighTier
	switch {
	case score >= 0.6 && highQualityValidated:
		return "high"
	case score >= 0.6:
		return "medium"
	case score >= 0.35:
		if validated {
			return "medium"
		}
		return "low"
	case score >= 0.15:
		return "low"
	default:
		return "none"
	}
}
