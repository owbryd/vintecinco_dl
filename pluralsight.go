package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	pluralsightBaseURL = "https://app.pluralsight.com"
	pluralsightUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

// PluralSightState holds authentication cookies and cached course list.
type PluralSightState struct {
	cookies   string
	xsrfToken string
	courses   []PluralSightCourse
}

// PluralSightCourse represents a single course in the user's history.
type PluralSightCourse struct {
	ID         string
	Name       string
	ImageURL   string
	Level      string
	FolderName string
}

var xsrfTokenRe = regexp.MustCompile(`XSRF-TOKEN=([^;]+)`)

// pluralsightHeaders returns common HTTP headers for Pluralsight API requests.
func (a *App) pluralsightHeaders() map[string]string {
	h := map[string]string{
		"User-Agent":       pluralsightUA,
		"Accept":           "application/json, text/plain, */*",
		"Accept-Language":  "pt-BR,pt;q=0.9",
		"Cookie":           a.pluralsight.cookies,
		"Sec-Fetch-Site":   "same-origin",
		"Sec-Fetch-Mode":   "cors",
		"Sec-Fetch-Dest":   "empty",
	}
	if a.pluralsight.xsrfToken != "" {
		h["x-xsrf-token"] = a.pluralsight.xsrfToken
	}
	return h
}

// pluralsightLogin validates Pluralsight session cookies.
// The "email" field is repurposed for the cookie string (same pattern as Hotmart).
func (a *App) pluralsightLogin(email, password string) map[string]interface{} {
	raw := strings.TrimSpace(email)
	if raw == "" {
		return map[string]interface{}{
			"success": false,
			"error":   "Paste your Pluralsight cookies from the browser DevTools (F12 > Network > copy cookie header).",
		}
	}

	// Handle multi-line "cookie: value" format from raw headers
	lines := strings.Split(raw, "\n")
	var parts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cleaned := regexp.MustCompile(`(?i)^cookie:\s*`).ReplaceAllString(line, "")
		parts = append(parts, cleaned)
	}
	cookies := strings.Join(parts, "; ")
	cookies = strings.ReplaceAll(cookies, "; ;", ";")

	// Extract XSRF-TOKEN
	var xsrf string
	if m := xsrfTokenRe.FindStringSubmatch(cookies); len(m) > 1 {
		xsrf = m[1]
	}

	a.pluralsight.cookies = cookies
	a.pluralsight.xsrfToken = xsrf

	// Validate by fetching history with limit=1
	hdrs := a.pluralsightHeaders()
	url := pluralsightBaseURL + "/learner/user/user-content-history?dataSource=v2&limit=1&labSort=progress"
	data, status, err := httpGetJSON(url, hdrs)
	if err != nil || status != 200 {
		a.pluralsight.cookies = ""
		a.pluralsight.xsrfToken = ""
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("Invalid cookies (HTTP %d)", status)}
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil || jsonArray(parsed, "collection") == nil {
		a.pluralsight.cookies = ""
		a.pluralsight.xsrfToken = ""
		return map[string]interface{}{"success": false, "error": "Could not validate cookies"}
	}

	return map[string]interface{}{"success": true}
}

// pluralsightGetCourses fetches the user's course history from Pluralsight.
func (a *App) pluralsightGetCourses() []map[string]interface{} {
	hdrs := a.pluralsightHeaders()
	url := pluralsightBaseURL + "/learner/user/user-content-history?dataSource=v2&limit=300&labSort=progress"

	data, status, err := httpGetJSON(url, hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	collection := jsonArray(parsed, "collection")
	var courses []PluralSightCourse

	for _, item := range collection {
		im, _ := item.(map[string]interface{})
		if im == nil {
			continue
		}
		content := jsonMap(im, "content")
		if content == nil {
			continue
		}

		courseID := jsonStr(content, "courseId")
		if courseID == "" {
			courseID = jsonStr(content, "id")
		}
		name := strings.Join(strings.Fields(jsonStr(content, "title")), " ")
		if name == "" {
			name = "Unnamed Course"
		}
		level := jsonStr(content, "level")

		imageURL := ""
		if img := jsonMap(content, "image"); img != nil {
			imageURL = jsonStr(img, "url")
		}

		folderName := sanitizeFilename(name)

		courses = append(courses, PluralSightCourse{
			ID:         courseID,
			Name:       name,
			ImageURL:   imageURL,
			Level:      level,
			FolderName: folderName,
		})
	}

	a.pluralsight.courses = courses

	result := make([]map[string]interface{}, len(courses))
	for i, c := range courses {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), c.FolderName))
		result[i] = map[string]interface{}{
			"name":        c.Name,
			"preview_url": c.ImageURL,
			"downloaded":  dlStatus != "none",
			"dl_status":   dlStatus,
		}
	}
	return result
}

// pluralsightGetTOC fetches the table of contents (modules + clips) for a course.
func (a *App) pluralsightGetTOC(courseID string) map[string]interface{} {
	hdrs := a.pluralsightHeaders()
	url := pluralsightBaseURL + "/course-player/api/v1/table-of-contents/course/" + courseID

	data, status, err := httpGetJSON(url, hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}
	return parsed
}

// pluralsightGetClipURL fetches the m3u8 stream URL for a specific clip.
func (a *App) pluralsightGetClipURL(clipID, versionID string) string {
	hdrs := a.pluralsightHeaders()
	hdrs["Content-Type"] = "application/json"
	hdrs["x-team"] = "video-services"

	url := pluralsightBaseURL + "/video/delivery/api/v1/clips/" + clipID + "/versions/" + versionID
	body := `{"online":true,"boundedContext":"course","preferredAudioLanguage":"en"}`

	resp, err := httpRequest("POST", url, strings.NewReader(body), hdrs, true)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return ""
	}

	// Find m3u8 URL, prefer English
	renditions := jsonArray(parsed, "renditions")
	for _, r := range renditions {
		rm, _ := r.(map[string]interface{})
		if rm == nil {
			continue
		}
		if jsonStr(rm, "format") == "m3u8" && jsonStr(rm, "languageCode") == "en" {
			urls := jsonArray(rm, "urls")
			if len(urls) > 0 {
				if um, ok := urls[0].(map[string]interface{}); ok {
					return jsonStr(um, "url")
				}
			}
		}
	}
	// Fallback: any m3u8
	for _, r := range renditions {
		rm, _ := r.(map[string]interface{})
		if rm == nil {
			continue
		}
		if jsonStr(rm, "format") == "m3u8" {
			urls := jsonArray(rm, "urls")
			if len(urls) > 0 {
				if um, ok := urls[0].(map[string]interface{}); ok {
					return jsonStr(um, "url")
				}
			}
		}
	}
	return ""
}

// runFfmpegCopy downloads an HLS stream using ffmpeg with stream copy (no re-encoding).
// Uses the same cancellation mechanism as yt-dlp (stored in a.ytdlpCmd).
func (a *App) runFfmpegCopy(m3u8URL, outputPath string, onProgress ProgressFn) error {
	cmd := exec.Command("ffmpeg", "-y",
		"-i", m3u8URL,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		outputPath,
	)
	cmd.SysProcAttr = ytdlpSysProcAttr()

	a.mu.Lock()
	a.ytdlpCmd = cmd
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.ytdlpCmd = nil
		a.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for {
		select {
		case err := <-done:
			if err == nil && onProgress != nil {
				onProgress(1000, 1000)
			}
			return err
		default:
			if a.cancel.Load() {
				cmd.Process.Kill()
				<-done
				os.Remove(outputPath)
				return fmt.Errorf("cancelled")
			}
			if onProgress != nil {
				onProgress(500, 1000)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// pluralsightClipDone checks if a clip .mp4 already exists and is non-empty.
func pluralsightClipDone(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// --- Download Orchestration ---

func (a *App) pluralsightDownloadOne(index int) bool {
	if index < 0 || index >= len(a.pluralsight.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course."})
		return false
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ok, retry := a.pluralsightDownloadOneAttempt(index)
		if ok {
			return true
		}
		if !retry || a.cancel.Load() {
			return false
		}
		if attempt < 3 {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": 0, "total": 0,
				"filename": fmt.Sprintf("Error, retrying... (%d/3)", attempt),
				"percent": 0,
			})
			time.Sleep(time.Duration(3*attempt) * time.Second)
		}
	}
	return false
}

func (a *App) pluralsightDownloadOneAttempt(index int) (ok bool, retryable bool) {
	defer func() {
		if r := recover(); r != nil {
			a.emit("dl_error", map[string]interface{}{
				"index": index, "message": fmt.Sprintf("Error: %v", r),
			})
			ok = false
			retryable = true
		}
	}()

	course := a.pluralsight.courses[index]
	courseDir := filepath.Join(a.platformDir(), course.FolderName)
	os.MkdirAll(courseDir, 0755)

	a.emit("dl_started", map[string]interface{}{"index": index, "folder": courseDir})

	// Clean .part files
	filepath.Walk(courseDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".part" {
			os.Remove(path)
		}
		return nil
	})

	// Get table of contents
	toc := a.pluralsightGetTOC(course.ID)
	if toc == nil {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Failed to fetch course content",
		})
		return false, true
	}

	modules := jsonArray(toc, "modules")
	if len(modules) == 0 {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Course has no modules"})
		return false, false
	}

	// Count total clips
	totalClips := 0
	for _, mod := range modules {
		mm, _ := mod.(map[string]interface{})
		totalClips += len(jsonArray(mm, "contentItems"))
	}

	currentClip := 0

	for mi, mod := range modules {
		mm, _ := mod.(map[string]interface{})
		modTitle := sanitizeFilename(jsonStr(mm, "title"))
		if modTitle == "" {
			modTitle = "Module"
		}
		modDir := filepath.Join(courseDir, fmt.Sprintf("%02d - %s", mi+1, modTitle))
		os.MkdirAll(modDir, 0755)

		clips := jsonArray(mm, "contentItems")
		for ci, clip := range clips {
			if a.cancel.Load() {
				a.emit("dl_cancelled", map[string]interface{}{"index": index})
				return false, false
			}

			currentClip++
			cm, _ := clip.(map[string]interface{})
			if cm == nil {
				continue
			}
			clipID := jsonStr(cm, "id")
			versionID := jsonStr(cm, "version")
			clipTitle := jsonStr(cm, "title")
			if clipTitle == "" {
				clipTitle = "Clip"
			}

			safeTitle := sanitizeFilename(fmt.Sprintf("%02d - %s", ci+1, clipTitle))
			outPath := filepath.Join(modDir, safeTitle+".mp4")

			// Skip if already downloaded
			if pluralsightClipDone(outPath) {
				a.emit("dl_progress", map[string]interface{}{
					"index": index, "current": currentClip, "total": totalClips,
					"filename": clipTitle + " (already downloaded)", "percent": 100,
				})
				continue
			}

			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentClip, "total": totalClips,
				"filename": clipTitle, "percent": 0,
			})

			if clipID == "" || versionID == "" {
				continue
			}

			// Get m3u8 URL
			m3u8URL := a.pluralsightGetClipURL(clipID, versionID)
			if m3u8URL == "" {
				continue
			}

			progress := func(dl, tb int64) {
				pct := 50.0
				if tb > 0 {
					pct = float64(dl) / float64(tb) * 100.0
				}
				a.emit("dl_progress", map[string]interface{}{
					"index": index, "current": currentClip, "total": totalClips,
					"filename": clipTitle, "percent": pct,
				})
			}

			// Download with ffmpeg (stream copy, no re-encoding)
			err := a.runFfmpegCopy(m3u8URL, outPath, progress)
			if err != nil {
				if strings.Contains(err.Error(), "cancelled") {
					a.emit("dl_cancelled", map[string]interface{}{"index": index})
					return false, false
				}
				// Clean up failed file
				os.Remove(outPath)
			}

			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentClip, "total": totalClips,
				"filename": clipTitle, "percent": 100,
			})
		}
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseDir})
	return true, false
}

func (a *App) pluralsightDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.pluralsightDownloadOne(idx) {
			if a.cancel.Load() {
				break
			}
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}
