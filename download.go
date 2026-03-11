package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// httpTransport is a shared transport for all HTTP requests, tuned for
// high-throughput downloads:
//   - TLS verification disabled (some CDNs use self-signed certs)
//   - Large connection pool (200 idle, 50 per host) to avoid reconnection overhead
//   - HTTP/2 enabled for multiplexed streams
//   - 256KB read/write buffers to reduce syscall overhead on large transfers
var httpTransport = &http.Transport{
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	MaxIdleConns:          200,
	MaxIdleConnsPerHost:   50,
	MaxConnsPerHost:       0,
	IdleConnTimeout:       90 * time.Second,
	DisableCompression:    false,
	ForceAttemptHTTP2:     true,
	ResponseHeaderTimeout: 30 * time.Second,
	WriteBufferSize:       256 * 1024,
	ReadBufferSize:        256 * 1024,
}

// httpClient is the default client for downloads. Timeout is disabled (0)
// because course videos can be very large and take a long time to download.
var httpClient = &http.Client{
	Timeout:   0,
	Transport: httpTransport,
}

// httpClientNoRedirect is used when we need to capture redirect URLs
// (e.g. Kiwify file download redirects). It returns the 3xx response
// instead of following the redirect automatically.
var httpClientNoRedirect = &http.Client{
	Timeout:   30 * time.Second,
	Transport: httpTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// httpRequest creates and executes an HTTP request with custom headers.
// Headers are set via direct map access (req.Header[k]) instead of
// req.Header.Set() to preserve exact casing — some APIs require
// non-standard header casing (e.g. CLIENT-TOKEN, KJB-DP).
func httpRequest(method, url string, body io.Reader, headers map[string]string, followRedirects bool) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header[k] = []string{v}
	}
	client := httpClient
	if !followRedirects {
		client = httpClientNoRedirect
	}
	return client.Do(req)
}

// httpGetJSON performs a GET request and returns the response body as bytes,
// the HTTP status code, and any error. Convenience wrapper for API calls
// that return JSON.
func httpGetJSON(url string, headers map[string]string) ([]byte, int, error) {
	resp, err := httpRequest("GET", url, nil, headers, true)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// ProgressFn is a callback for reporting download progress.
// downloaded = bytes received so far, total = content length (-1 if unknown).
type ProgressFn func(downloaded, total int64)

// downloadToFile downloads a URL to a local file with progress reporting and
// cancellation support. It uses a 1MB buffered writer and 1MB read buffer to
// minimize syscalls. The file is written to a .part temp file and renamed on
// completion to prevent partial files from being mistaken as complete.
//
// Cancellation is implemented via context: a goroutine watches a.cancel and
// cancels the HTTP request context, which closes the TCP connection immediately.
func (a *App) downloadToFile(url, dest string, headers map[string]string, onProgress ProgressFn) error {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	// Monitor a.cancel in background and propagate to HTTP context.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
				if a.cancel.Load() {
					cancelCtx()
					return
				}
			}
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header[k] = []string{v}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil || a.cancel.Load() {
			return fmt.Errorf("cancelled")
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	tempPath := dest + ".part"
	f, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1024*1024) // 1MB buffered writer

	var downloaded int64
	buf := make([]byte, 1024*1024) // 1MB read buffer
	lastReport := time.Now()
	var writeErr error

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr = bw.Write(buf[:n]); writeErr != nil {
				f.Close()
				os.Remove(tempPath)
				return writeErr
			}
			downloaded += int64(n)
			// Throttle progress callbacks to every 200ms to avoid flooding the UI
			if onProgress != nil && time.Since(lastReport) > 200*time.Millisecond {
				onProgress(downloaded, total)
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			os.Remove(tempPath)
			// Distinguish context cancellation from real read errors.
			if ctx.Err() != nil || a.cancel.Load() {
				return fmt.Errorf("cancelled")
			}
			return readErr
		}
	}

	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}
	f.Close()

	if downloaded == 0 {
		os.Remove(tempPath)
		return fmt.Errorf("empty download")
	}

	if onProgress != nil {
		onProgress(downloaded, total)
	}

	return os.Rename(tempPath, dest)
}

// downloadToFileSimple downloads a file without cancellation support.
// Intended for small files (thumbnails, attachments) where cancel overhead
// is not needed. Uses io.CopyBuffer with a 1MB buffer.
func downloadToFileSimple(url, dest string, headers map[string]string) error {
	resp, err := httpRequest("GET", url, nil, headers, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	tempPath := dest + ".part"
	f, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(f, resp.Body, make([]byte, 1024*1024)); err != nil {
		f.Close()
		os.Remove(tempPath)
		return err
	}
	f.Close()

	info, _ := os.Stat(tempPath)
	if info == nil || info.Size() == 0 {
		os.Remove(tempPath)
		return fmt.Errorf("empty download")
	}

	return os.Rename(tempPath, dest)
}

// percentRegex matches percentage strings like "45.2%" in yt-dlp output.
var percentRegex = regexp.MustCompile(`(\d+\.?\d*)%`)

// runYtdlpArgs runs yt-dlp with the given raw arguments and streams progress.
func (a *App) runYtdlpArgs(args []string, onProgress ProgressFn) error {
	cmd := exec.Command("yt-dlp", args...)
	return a.runYtdlpExec(cmd, onProgress)
}

// runYtdlp runs yt-dlp with optimized default arguments for downloading course
// videos. Default settings include:
//   - -N 64: 64 concurrent fragment downloads for maximum speed
//   - --http-chunk-size 10M: split downloads into 10MB chunks
//   - --throttled-rate 100K: auto-reconnect if speed drops below 100KB/s
//   - --retries/--fragment-retries 10: retry on network errors
//
// extraArgs are appended before the output template and URL.
func (a *App) runYtdlp(outputTemplate, videoURL string, onProgress ProgressFn, extraArgs ...string) error {
	args := []string{
		"--no-check-certificates",
		"--progress", "--newline",
		"-f", "best[height<=720]/bestvideo[height<=720]+bestaudio/best",
		"-N", "64",
		"--retries", "10",
		"--fragment-retries", "10",
		"--buffer-size", "64K",
		"--http-chunk-size", "10M",
		"--throttled-rate", "100K",
		"--merge-output-format", "mp4",
	}
	args = append(args, extraArgs...)
	args = append(args, "-o", outputTemplate, videoURL)
	return a.runYtdlpArgs(args, onProgress)
}

// runYtdlpExec starts a yt-dlp command and parses its stdout+stderr for progress
// percentages. The process is stored in a.ytdlpCmd so it can be killed on cancel.
// Progress is normalized to 0-1000 range for the ProgressFn callback.
func (a *App) runYtdlpExec(cmd *exec.Cmd, onProgress ProgressFn) error {
	cmd.SysProcAttr = ytdlpSysProcAttr()
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Merge stdout and stderr into a single reader so the scanner sees all output.
	// yt-dlp writes progress to stdout and errors to stderr.
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(pw, stdoutPipe) }()
	go func() { defer wg.Done(); io.Copy(pw, stderrPipe) }()
	go func() { wg.Wait(); pw.Close() }()

	// Store reference so Cancel() can kill the process
	a.mu.Lock()
	a.ytdlpCmd = cmd
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.ytdlpCmd = nil
		a.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		pr.Close()
		return err
	}

	scanner := bufio.NewScanner(pr)
	scanner.Split(scanLinesOrCR)

	lastLine := ""
	for scanner.Scan() {
		if a.cancel.Load() {
			cmd.Process.Kill()
			cmd.Wait()
			pr.Close()
			return fmt.Errorf("cancelled")
		}
		line := scanner.Text()
		if strings.TrimSpace(line) != "" {
			lastLine = strings.TrimSpace(line)
		}
		if m := percentRegex.FindStringSubmatch(line); m != nil {
			pct, _ := strconv.ParseFloat(m[1], 64)
			if onProgress != nil {
				// Scale 0-100% to 0-1000 for integer precision
				onProgress(int64(pct*10), 1000)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		if lastLine != "" {
			return fmt.Errorf("%s", lastLine)
		}
		return err
	}
	return nil
}

// scanLinesOrCR is a bufio.SplitFunc that splits on \n or \r.
// yt-dlp uses \r for in-place progress updates, so we need to handle
// both line endings to capture each progress line.
func scanLinesOrCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// cleanYtdlpTempFiles removes the output file and any yt-dlp temporary files
// left behind after a failed or cancelled download. yt-dlp creates intermediate
// files like video.mp4.part, video.f248.mp4, video.f251.m4a, video.temp.mp4, etc.
func cleanYtdlpTempFiles(outputPath string) {
	os.Remove(outputPath)
	os.Remove(outputPath + ".part")

	dir := filepath.Dir(outputPath)
	base := strings.TrimSuffix(filepath.Base(outputPath), filepath.Ext(outputPath))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, base+".f") || strings.HasPrefix(name, base+".temp") {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// extractFilenameFromURL extracts the filename component from a URL path,
// stripping any query parameters. Returns "file" if no name can be determined.
func extractFilenameFromURL(url string) string {
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return "file"
	}
	name := parts[len(parts)-1]
	if idx := strings.Index(name, "?"); idx != -1 {
		name = name[:idx]
	}
	if name == "" {
		return "file"
	}
	return name
}
