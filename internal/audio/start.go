// Package audio plays short UI sounds (e.g. the startup chime that
// accompanies the Buddha banner). It is intentionally best-effort:
// missing players or audio devices must never block or error the TUI.
package audio

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

//go:embed start.mp3
var startMP3 []byte

var (
	extractOnce sync.Once
	extractPath string
	extractErr  error
)

// extract writes the embedded mp3 to a temp file and returns its path.
// Safe to call repeatedly — the file is written only on first call.
func extract() (string, error) {
	extractOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "banya-cli")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			extractErr = err
			return
		}
		extractPath = filepath.Join(dir, "start.mp3")
		extractErr = os.WriteFile(extractPath, startMP3, 0o644)
	})
	return extractPath, extractErr
}

// PlayStart plays the startup sound in the background. The call returns
// immediately; errors are silent (we never want a missing audio player
// to interrupt the TUI).
func PlayStart() {
	go func() {
		path, err := extract()
		if err != nil {
			return
		}
		name, args := playerCommand(path)
		if name == "" {
			return
		}
		cmd := exec.Command(name, args...)
		// Detach stdio so the player can't write into the TUI.
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		_ = cmd.Run()
	}()
}

// playerCommand picks a native audio player for the current OS. Returns
// an empty name when nothing suitable is on PATH; callers then skip
// playback silently.
func playerCommand(path string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "afplay", []string{path}
	case "linux":
		for _, candidate := range [][]string{
			{"paplay", path},
			{"aplay", path},
			{"play", "-q", path},
			{"mpg123", "-q", path},
			{"ffplay", "-autoexit", "-nodisp", "-loglevel", "quiet", path},
		} {
			if _, err := exec.LookPath(candidate[0]); err == nil {
				return candidate[0], candidate[1:]
			}
		}
		return "", nil
	case "windows":
		return "powershell", []string{
			"-NoProfile",
			"-Command",
			"(New-Object Media.SoundPlayer '" + path + "').PlaySync()",
		}
	}
	return "", nil
}
