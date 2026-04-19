// Package domain provides a capability-coverage signal for banya-cli.
//
// Motivation
// ──────────
// Our agent is benchmark-validated on SWE-bench Lite (12 Python repos:
// scientific, web, viz, tooling). That's a narrow slice of what real
// users will throw at us — frontend/TS, Rust systems, mobile, shaders,
// infra-as-code, etc. are all out-of-distribution.
//
// Pretending the benchmark number applies universally is dishonest UX
// and risks silent quality drops. Instead we:
//
//   1. Maintain a static map of "domains" we have measured (Coverage).
//   2. At run time, scan the workspace for static signals (file
//      extensions + marker files + keyword signals) and compute a
//      Match score per domain (domain.Scan).
//   3. Surface the best-matching domain + confidence tier to the agent
//      (meta event) and to the user (human-readable warning when the
//      task falls outside every validated domain).
//
// No LLM calls here — everything is deterministic static file inspection
// so the signal is available before the first agent turn.
package domain

import (
	_ "embed"
	"strings"
)

// Domain represents one distinct capability slice we can talk about —
// "Python scientific computing", "Python web framework", etc. The set
// is curated, not auto-derived: adding a new domain means running a
// real benchmark against it first, so the coverage.yaml stays honest.
type Domain struct {
	// Stable key (snake_case). Used in meta events and reports.
	Key string `yaml:"key"`
	// Short human-readable label.
	Label string `yaml:"label"`
	// Description rendered in warnings. One sentence.
	Description string `yaml:"description"`
	// Canonical file extensions. A workspace containing any of these
	// contributes to the match score.
	Extensions []string `yaml:"extensions"`
	// Marker files (exact basename match). E.g. "package.json", "Cargo.toml".
	Markers []string `yaml:"markers"`
	// Lower-case keyword substrings scanned in requirements/pyproject/
	// Cargo/package.json/etc. Hits these and we raise the match score.
	Keywords []string `yaml:"keywords"`
	// Benchmarks we've run against this domain. An empty list means
	// "unvalidated" — we can still match the domain but we won't claim
	// a measured pass rate.
	Benchmarks []Benchmark `yaml:"benchmarks"`
}

// Benchmark is a measured evaluation result, used to render an honest
// "we've measured X% here" string. Date is ISO-8601; passRate is 0..1.
type Benchmark struct {
	Name     string  `yaml:"name"`     // e.g. "SWE-bench Lite"
	Sample   int     `yaml:"sample"`   // tasks measured
	PassRate float64 `yaml:"passRate"` // 0..1
	Date     string  `yaml:"date"`     // "2026-04-20"
}

// coverage is the single source of truth. Literal (not YAML) because
// inlining avoids a runtime parse / file-load path and keeps the binary
// self-contained. When the list grows past ~20 entries we'll flip this
// to an embed.FS + yaml.Unmarshal.
var coverage = []Domain{
	{
		Key:         "python_scientific_astropy",
		Label:       "Python scientific — astropy",
		Description: "Astronomy/coord-system specific. astropy.Table, Quantity, Modeling, WCS.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.py", "setup.cfg"},
		Keywords:    []string{"astropy", "astroquery", "specutils"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (astropy)", Sample: 1, PassRate: 1.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_scientific_sklearn",
		Label:       "Python scientific — scikit-learn",
		Description: "Classical ML — estimators, pipelines, preprocessors, cross-validation.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.py", "setup.cfg"},
		Keywords:    []string{"scikit-learn", "sklearn", "imblearn"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (sklearn)", Sample: 1, PassRate: 0.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_scientific_sympy",
		Label:       "Python scientific — sympy",
		Description: "Symbolic math. Expressions, simplify, solvers, integrators.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.py", "setup.cfg"},
		Keywords:    []string{"sympy"},
		Benchmarks:  nil,
	},
	{
		Key:         "python_scientific_xarray",
		Label:       "Python scientific — xarray/pandas",
		Description: "Labelled-axis N-D arrays, dataset operations, netCDF.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.py", "setup.cfg"},
		Keywords:    []string{"xarray", "pandas", "dask"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (xarray)", Sample: 1, PassRate: 0.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_scientific_generic",
		Label:       "Python scientific — generic (numpy/scipy)",
		Description: "Umbrella entry for numpy/scipy workloads that don't resolve to a specific library.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.py", "setup.cfg"},
		Keywords:    []string{"numpy", "scipy"},
		Benchmarks:  nil,
	},
	{
		Key:         "python_web",
		Label:       "Python web framework",
		Description: "HTTP request/response, ORM, routing. django / flask / requests / fastapi family.",
		Extensions:  []string{".py"},
		Markers:     []string{"manage.py", "wsgi.py", "asgi.py", "requirements.txt"},
		Keywords: []string{
			"django", "flask", "fastapi", "requests",
			"werkzeug", "sqlalchemy", "gunicorn", "uvicorn",
		},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (django+flask+requests)", Sample: 3, PassRate: 0.33, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_dataviz",
		Label:       "Python data visualisation",
		Description: "Plotting, figures, statistical graphics. matplotlib / seaborn / plotly family.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml"},
		Keywords:    []string{"matplotlib", "seaborn", "plotly", "bokeh", "altair", "pyplot"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (matplotlib+seaborn)", Sample: 2, PassRate: 0.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_tooling_pytest",
		Label:       "Python tooling — pytest",
		Description: "Test runner internals, fixtures, collectors, plugins.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "tox.ini", "setup.cfg"},
		Keywords:    []string{"pytest", "_pytest", "pluggy"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (pytest)", Sample: 1, PassRate: 0.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_tooling_pylint",
		Label:       "Python tooling — pylint / lint",
		Description: "Static analysis, checkers, AST traversal.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.cfg"},
		Keywords:    []string{"pylint", "astroid", "flake8", "ruff", "mccabe"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (pylint)", Sample: 1, PassRate: 1.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_tooling_sphinx",
		Label:       "Python tooling — sphinx / docs",
		Description: "Documentation generators, directives, autodoc, RST.",
		Extensions:  []string{".py", ".rst"},
		Markers:     []string{"pyproject.toml", "conf.py", "setup.cfg"},
		Keywords:    []string{"sphinx", "docutils", "myst"},
		Benchmarks: []Benchmark{
			{Name: "SWE-bench Lite (sphinx)", Sample: 1, PassRate: 1.0, Date: "2026-04-20"},
		},
	},
	{
		Key:         "python_tooling_generic",
		Label:       "Python tooling — other (mypy / black / isort)",
		Description: "Type-checkers, formatters, import sorters.",
		Extensions:  []string{".py"},
		Markers:     []string{"pyproject.toml", "setup.cfg"},
		Keywords:    []string{"mypy", "black", "isort", "autopep8"},
		Benchmarks:  nil,
	},
	{
		Key:         "typescript_react",
		Label:       "TypeScript frontend — React",
		Description: "React / Next / CRA / JSX-heavy workspaces.",
		Extensions:  []string{".ts", ".tsx", ".jsx", ".js", ".css", ".scss"},
		Markers:     []string{"package.json", "tsconfig.json", "next.config.js", "next.config.mjs"},
		Keywords:    []string{"react", "next", "redux", "zustand", "recoil", "react-dom"},
		Benchmarks:  nil, // webbench subset=react will populate this via auto-record.
	},
	{
		Key:         "typescript_vue",
		Label:       "TypeScript frontend — Vue",
		Description: "Vue 2/3 / Nuxt SFC projects.",
		Extensions:  []string{".ts", ".js", ".vue", ".css", ".scss"},
		Markers:     []string{"package.json", "tsconfig.json", "vite.config.ts", "nuxt.config.ts"},
		Keywords:    []string{"vue", "nuxt", "pinia", "vuex"},
		Benchmarks:  nil,
	},
	{
		Key:         "typescript_angular",
		Label:       "TypeScript frontend — Angular",
		Description: "Angular CLI workspaces, RxJS-heavy, decorators.",
		Extensions:  []string{".ts", ".html", ".scss"},
		Markers:     []string{"angular.json", "package.json", "tsconfig.json"},
		Keywords:    []string{"@angular/core", "@angular/cli", "rxjs"},
		Benchmarks:  nil,
	},
	{
		Key:         "typescript_svelte",
		Label:       "TypeScript frontend — Svelte",
		Description: "SvelteKit / Svelte 4/5 projects.",
		Extensions:  []string{".ts", ".js", ".svelte", ".css"},
		Markers:     []string{"svelte.config.js", "svelte.config.ts", "package.json"},
		Keywords:    []string{"svelte", "sveltekit"},
		Benchmarks:  nil,
	},
	{
		Key:         "typescript_frontend_generic",
		Label:       "TypeScript / JavaScript frontend — other",
		Description: "Other browser UI stacks (Solid, Lit, Alpine, plain DOM).",
		Extensions:  []string{".ts", ".tsx", ".jsx", ".js", ".mjs", ".css", ".scss"},
		Markers:     []string{"package.json", "tsconfig.json", "vite.config.ts"},
		Keywords:    []string{"solid-js", "lit", "alpinejs", "tailwind", "vite", "webpack"},
		Benchmarks:  nil,
	},
	{
		Key:         "typescript_backend",
		Label:       "TypeScript / Node backend & libraries",
		Description: "Node servers, APIs, edge workers, and ecosystem libraries (Babel, Axios, Immutable, Three, Preact). Express / Fastify / Nest / tRPC / Hono family + Node libraries shipped as packages.",
		Extensions:  []string{".ts", ".js", ".mjs"},
		Markers:     []string{"package.json", "tsconfig.json"},
		Keywords: []string{
			"express", "fastify", "nestjs", "trpc", "hono", "prisma", "drizzle",
			"babel", "@babel/core", "axios", "immutable", "three", "preact",
			"docusaurus", "koa",
		},
		Benchmarks: nil, // populated by SWE-bench Multilingual run
	},
	{
		Key:         "go_backend",
		Label:       "Go backend / CLI",
		Description: "Go services, CLIs, tooling. net/http / gin / cobra / bubbletea + infra (Caddy / Terraform / Prometheus / Hugo).",
		Extensions:  []string{".go"},
		Markers:     []string{"go.mod", "go.sum"},
		Keywords: []string{
			"cobra", "gin", "echo", "gin-gonic", "bubbletea", "grpc",
			"kubernetes", "caddy", "terraform", "prometheus", "hugo",
			"hashicorp", "fiber",
		},
		Benchmarks: nil, // populated by SWE-bench Multilingual run
	},
	{
		Key:         "rust_systems",
		Label:       "Rust systems / CLI",
		Description: "Rust crates — CLIs, async services, embedded. cargo / tokio / axum / clap family.",
		Extensions:  []string{".rs"},
		Markers:     []string{"Cargo.toml", "Cargo.lock"},
		Keywords:    []string{"tokio", "axum", "clap", "serde", "anyhow", "reqwest", "hyper", "actix"},
		Benchmarks:  nil,
	},
	{
		Key:         "java_backend",
		Label:       "Java backend",
		Description: "JVM-era backend — Spring / Lombok / Druid / Lucene / Gson / JavaParser. Also covers data-processing and parser internals.",
		Extensions:  []string{".java", ".kt", ".scala"},
		Markers:     []string{"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"},
		Keywords: []string{
			"spring", "spring-boot", "springframework",
			"gson", "jackson", "lombok",
			"lucene", "druid", "hibernate",
			"javaparser", "rxjava", "mockito",
		},
		Benchmarks: nil, // populated by SWE-bench Multilingual run
	},
	{
		Key:         "cpp_systems",
		Label:       "C / C++ systems",
		Description: "Low-level libraries, performance-critical code, embedded.",
		Extensions:  []string{".c", ".cpp", ".cc", ".cxx", ".h", ".hpp"},
		Markers:     []string{"CMakeLists.txt", "Makefile", "meson.build", "BUILD"},
		Keywords:    nil,
		Benchmarks:  nil,
	},
	{
		Key:         "mobile_swift",
		Label:       "iOS / macOS Swift",
		Description: "SwiftUI, UIKit, iOS apps, AppKit.",
		Extensions:  []string{".swift"},
		Markers:     []string{"Package.swift", "Podfile", "*.xcodeproj"},
		Keywords:    []string{"swiftui", "uikit", "combine"},
		Benchmarks:  nil,
	},
	{
		Key:         "mobile_kotlin",
		Label:       "Android Kotlin",
		Description: "Android apps, Jetpack Compose, Gradle.",
		Extensions:  []string{".kt", ".kts"},
		Markers:     []string{"build.gradle.kts", "build.gradle", "settings.gradle.kts"},
		Keywords:    []string{"jetpack", "compose", "koin", "hilt"},
		Benchmarks:  nil,
	},
	{
		Key:         "devops_infra",
		Label:       "DevOps / infrastructure-as-code",
		Description: "Terraform / Kubernetes / Docker / Helm configs.",
		Extensions:  []string{".tf", ".yaml", ".yml", ".hcl"},
		Markers:     []string{"Dockerfile", "docker-compose.yml", "docker-compose.yaml", "kustomization.yaml"},
		Keywords:    []string{"terraform", "kubernetes", "helm", "argocd", "ansible"},
		Benchmarks:  nil,
	},
}

// All returns the static coverage list. Callers should treat the result
// as read-only.
func All() []Domain {
	out := make([]Domain, len(coverage))
	copy(out, coverage)
	return out
}

// ByKey looks up a domain by its stable key. Returns (Domain{}, false)
// when unknown.
func ByKey(key string) (Domain, bool) {
	for _, d := range coverage {
		if d.Key == key {
			return d, true
		}
	}
	return Domain{}, false
}

// HasValidation returns true when this domain has at least one recorded
// benchmark — distinguishes "we've measured this" from "matched but
// untested". Used by confidence-tier decisions in run.go.
func (d Domain) HasValidation() bool {
	return len(d.Benchmarks) > 0
}

// BestPassRate returns the highest recorded pass rate (or 0 when none).
// Used for the human-readable confidence line.
func (d Domain) BestPassRate() float64 {
	var best float64
	for _, b := range d.Benchmarks {
		if b.PassRate > best {
			best = b.PassRate
		}
	}
	return best
}

// TotalSample returns the summed `Sample` across all recorded
// benchmarks. Used by the min-sample tier demotion rule: a domain
// with a single tiny benchmark (n=1) shouldn't be treated as "high
// confidence" just because that one task passed.
func (d Domain) TotalSample() int {
	var n int
	for _, b := range d.Benchmarks {
		n += b.Sample
	}
	return n
}

// MinSampleForHighTier is the threshold below which a "validated" domain
// gets demoted to medium. Five is a pragmatic lower bound — Wilson 95%
// CI on 5/5 is still wide but it at least rules out "lucky single task".
const MinSampleForHighTier = 5

// normalizeExt lowercases + ensures a leading dot. ".PY" / "py" / ".Py"
// all collapse to ".py".
func normalizeExt(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return ""
	}
	if !strings.HasPrefix(e, ".") {
		e = "." + e
	}
	return e
}
