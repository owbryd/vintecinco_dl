package main

import (
	"archive/tar"
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ulikunitz/xz"
)

// depsBinDir returns the directory where auto-downloaded tools are stored.
//
//	Windows: %LOCALAPPDATA%\vintecinco_dl\bin
//	Linux:   $HOME/.local/share/vintecinco_dl/bin
func depsBinDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "vintecinco_dl", "bin")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "vintecinco_dl", "bin")
}

// injectBinDir prepends depsBinDir() to the process PATH so that
// exec.LookPath and os/exec transparently find our bundled tools.
// Called once on app startup.
func injectBinDir() {
	dir := depsBinDir()
	prev := os.Getenv("PATH")
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+prev)
}

// ytdlpBin returns the platform-specific yt-dlp executable name.
func ytdlpBin() string {
	if runtime.GOOS == "windows" {
		return "yt-dlp.exe"
	}
	return "yt-dlp"
}

// ffmpegBin returns the platform-specific ffmpeg executable name.
func ffmpegBin() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

// DepStatus is sent as a "dep_progress" event to the frontend.
type DepStatus struct {
	Name    string  `json:"name"`
	Percent float64 `json:"percent"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`
}

// depEmit converts DepStatus to the map[string]interface{} that a.emit expects.
func (a *App) depEmit(s DepStatus) {
	m := map[string]interface{}{
		"name":    s.Name,
		"percent": s.Percent,
		"done":    s.Done,
	}
	if s.Error != "" {
		m["error"] = s.Error
	}
	a.emit("dep_progress", m)
}

// EnsureDeps checks for yt-dlp and ffmpeg in depsBinDir(), downloading
// them automatically if absent. Progress is reported via "dep_progress"
// runtime events so the frontend can show a setup screen.
// Returns nil when all dependencies are ready.
func (a *App) EnsureDeps() error {
	dir := depsBinDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create bin dir: %w", err)
	}

	type dep struct {
		name string
		file string
		dl   func(string, func(float64)) error
	}
	deps := []dep{
		{"yt-dlp", ytdlpBin(), a.dlYtdlp},
		{"ffmpeg", ffmpegBin(), a.dlFfmpeg},
	}

	for _, d := range deps {
		dest := filepath.Join(dir, d.file)
		if _, err := os.Stat(dest); err == nil {
			// Already installed — emit instant done so the UI can show ✓
			a.depEmit(DepStatus{Name: d.name, Percent: 100, Done: true})
			continue
		}
		a.depEmit(DepStatus{Name: d.name, Percent: 0})
		if err := d.dl(dest, func(pct float64) {
			a.depEmit(DepStatus{Name: d.name, Percent: pct})
		}); err != nil {
			a.depEmit(DepStatus{Name: d.name, Error: err.Error()})
			return fmt.Errorf("%s: %w", d.name, err)
		}
		if runtime.GOOS != "windows" {
			_ = os.Chmod(dest, 0755)
		}
		a.depEmit(DepStatus{Name: d.name, Percent: 100, Done: true})
	}
	return nil
}

// dlYtdlp downloads the latest yt-dlp binary for the current OS.
func (a *App) dlYtdlp(dest string, onProgress func(float64)) error {
	var url string
	switch runtime.GOOS {
	case "windows":
		url = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	case "linux":
		url = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return depsFetch(url, dest, onProgress)
}

// dlFfmpeg downloads ffmpeg for the current OS and extracts the executable.
func (a *App) dlFfmpeg(dest string, onProgress func(float64)) error {
	switch runtime.GOOS {
	case "windows":
		return depsFfmpegWindows(dest, onProgress)
	case "linux":
		return depsFfmpegLinux(dest, onProgress)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// depsFfmpegLinux downloads the BtbN Linux64 build (.tar.xz) and extracts
// just the ffmpeg binary into dest.
func depsFfmpegLinux(dest string, onProgress func(float64)) error {
	const url = "https://github.com/BtbN/FFmpeg-Builds/releases/latest/download/ffmpeg-master-latest-linux64-gpl.tar.xz"
	tarPath := dest + ".tar.xz"
	defer os.Remove(tarPath)

	// Download phase → 0-85%
	if err := depsFetch(url, tarPath, func(pct float64) {
		onProgress(pct * 0.85)
	}); err != nil {
		return err
	}
	onProgress(87)

	// Extract ffmpeg binary from the tar.xz archive
	return depsExtractFromTarXz(tarPath, "ffmpeg", dest)
}

// depsFfmpegWindows downloads the BtbN Windows build (~56 MB zip) and
// extracts ffmpeg.exe into dest.
func depsFfmpegWindows(dest string, onProgress func(float64)) error {
	const url = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"
	zipPath := dest + ".zip"
	defer os.Remove(zipPath)

	// Download phase → 0-90%
	if err := depsFetch(url, zipPath, func(pct float64) {
		onProgress(pct * 0.9)
	}); err != nil {
		return err
	}
	onProgress(92)

	// Extract ffmpeg.exe from anywhere inside the zip
	return depsExtractFromZip(zipPath, "ffmpeg.exe", dest)
}

// depsFetch downloads url to dest, calling onProgress with 0-100 values.
func depsFetch(url, dest string, onProgress func(float64)) error {
	resp, err := http.Get(url) //nolint:gosec // URL is a compile-time constant
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	total := resp.ContentLength
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	var writeErr error
	defer func() {
		f.Close()
		if writeErr != nil {
			os.Remove(dest)
		}
	}()

	var downloaded int64
	buf := make([]byte, 512*1024) // 512 KB chunks
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr = f.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)
			if total > 0 && onProgress != nil {
				onProgress(float64(downloaded) / float64(total) * 100)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

// depsExtractFromZip finds targetFile (by basename, case-insensitive) anywhere
// inside archive and writes it to dest.
func depsExtractFromZip(archive, targetFile, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if !f.FileInfo().IsDir() && strings.EqualFold(filepath.Base(f.Name), targetFile) {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, rc)
			return err
		}
	}
	return fmt.Errorf("%s not found in zip", targetFile)
}// depsExtractFromTarXz finds targetFile (by basename) anywhere inside a
// .tar.xz archive and writes it to dest.
func depsExtractFromTarXz(archivePath, targetFile, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	xzr, err := xz.NewReader(f)
	if err != nil {
		return err
	}

	tr := tar.NewReader(xzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !hdr.FileInfo().IsDir() && filepath.Base(hdr.Name) == targetFile {
			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, tr)
			return err
		}
	}
	return fmt.Errorf("%s not found in tar archive", targetFile)
}
