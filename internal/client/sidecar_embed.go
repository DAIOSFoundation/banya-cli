package client

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// bundledSidecars holds banya-core binaries embedded at build time. The
// build system (make build) populates these files by copying the core
// SEA outputs from ../banya-core/dist/banya-core-<os>-<arch>. If a file
// for the current platform is present, the cli auto-installs it on first
// run, avoiding a separate core install step.
//
// An empty embed directory is valid — ResolveSidecarPath will fall back
// to the explicit/env/XDG/PATH search and surface a clear error.
//
//go:embed all:embedded_sidecar
var bundledSidecars embed.FS

const embeddedDir = "embedded_sidecar"

// InstallEmbeddedSidecar extracts the bundled sidecar for the current
// platform into $XDG_DATA_HOME/banya/bin and returns its path. Returns
// os.ErrNotExist when no bundle is embedded for this platform — callers
// should fall back to other resolution strategies.
func InstallEmbeddedSidecar() (string, error) {
	binName := platformBinaryName()
	entry, err := bundledSidecars.ReadFile(filepath.ToSlash(filepath.Join(embeddedDir, binName)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("read embedded sidecar: %w", err)
	}
	if len(entry) < 1024 {
		// A sentinel placeholder (e.g. .gitkeep) — treat as absent.
		return "", os.ErrNotExist
	}

	dir, err := sidecarInstallDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create install dir: %w", err)
	}
	dst := filepath.Join(dir, binName)

	// Skip if already installed with matching checksum.
	want := sha256.Sum256(entry)
	if existing, err := os.ReadFile(dst); err == nil {
		have := sha256.Sum256(existing)
		if have == want {
			return dst, nil
		}
	}

	tmp := dst + ".tmp-" + hex.EncodeToString(want[:4])
	if err := os.WriteFile(tmp, entry, 0o755); err != nil {
		return "", fmt.Errorf("write sidecar: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("rename sidecar: %w", err)
	}
	return dst, nil
}

func sidecarInstallDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "banya", "bin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "banya", "bin"), nil
}

func platformBinaryName() string {
	name := fmt.Sprintf("banya-core-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}
