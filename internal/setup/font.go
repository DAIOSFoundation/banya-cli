// Package setup handles first-run setup tasks like Nerd Font installation.
package setup

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// NerdFont defines a downloadable Nerd Font.
type NerdFont struct {
	Name    string // display name
	ID      string // filename slug on GitHub releases
	Version string
}

// DefaultFont is the font we install by default.
var DefaultFont = NerdFont{
	Name:    "JetBrainsMono Nerd Font",
	ID:      "JetBrainsMono",
	Version: "v3.4.0",
}

// fontsDir returns the user font directory for the current OS.
func fontsDir() string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Fonts")
	default: // linux and others
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", "fonts")
	}
}

// HasNerdFont checks if any Nerd Font is installed by looking for the
// Powerline glyph range in fc-list output (Linux) or font file names.
func HasNerdFont() bool {
	// Method 1: fc-list (Linux)
	if out, err := exec.Command("fc-list").Output(); err == nil {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "nerd") || strings.Contains(lower, "powerline") {
			return true
		}
	}

	// Method 2: check our install directory for known font files
	dir := fontsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		lower := strings.ToLower(e.Name())
		if strings.Contains(lower, "nerd") || strings.Contains(lower, "powerline") {
			return true
		}
	}

	return false
}

// InstallNerdFont downloads and installs the given Nerd Font.
// Returns nil on success.
func InstallNerdFont(font NerdFont, progress func(msg string)) error {
	dir := fontsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create font directory: %w", err)
	}

	// Download URL
	url := fmt.Sprintf(
		"https://github.com/ryanoasis/nerd-fonts/releases/download/%s/%s.tar.xz",
		font.Version, font.ID,
	)

	progress(fmt.Sprintf("Downloading %s...", font.Name))

	archivePath := filepath.Join(os.TempDir(), font.ID+".tar.xz")
	defer os.Remove(archivePath)

	if err := downloadFile(url, archivePath); err != nil {
		return fmt.Errorf("download font: %w", err)
	}

	progress("Extracting fonts...")

	// Extract to font directory
	cmd := exec.Command("tar", "xf", archivePath, "-C", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract font: %w\n%s", err, string(out))
	}

	// Update font cache (Linux)
	progress("Updating font cache...")
	if runtime.GOOS == "linux" {
		exec.Command("fc-cache", "-f").Run()
	}

	progress(fmt.Sprintf("%s installed successfully!", font.Name))
	return nil
}

// downloadFile downloads a URL to a local file.
func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// SetGnomeTerminalFont configures GNOME Terminal to use the given font.
// This is a best-effort operation — silently ignored if not using GNOME Terminal.
func SetGnomeTerminalFont(fontName string, size int) {
	// Get default profile
	out, err := exec.Command("gsettings", "get",
		"org.gnome.Terminal.ProfilesList", "default").Output()
	if err != nil {
		return
	}
	profile := strings.Trim(strings.TrimSpace(string(out)), "'")
	if profile == "" {
		return
	}

	ppath := fmt.Sprintf("/org/gnome/terminal/legacy/profiles:/:%s/", profile)

	fontSpec := fmt.Sprintf("%s %d", fontName, size)

	exec.Command("gsettings", "set",
		"org.gnome.Terminal.Legacy.Profile:"+ppath, "use-system-font", "false").Run()
	exec.Command("gsettings", "set",
		"org.gnome.Terminal.Legacy.Profile:"+ppath, "font", fontSpec).Run()
}
