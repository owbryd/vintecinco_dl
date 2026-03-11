package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	mcBaseURL    = "https://www.masterclass.com"
	mcEdgeURL    = "https://edge.masterclass.com"
	mcEdgeAPIKey = "b9517f7d8d1f48c2de88100f2c13e77a9d8e524aed204651acca65202ff5c6cb9244c045795b1fafda617ac5eb0a6c50"
)

// MasterClassState holds authentication cookies and cached course list.
type MasterClassState struct {
	cookies   string
	profileID string
	courses   []MasterClassCourse
}

// MasterClassCourse represents a single course from the user's watchlist.
type MasterClassCourse struct {
	ID         int
	Title      string
	Slug       string
	Instructor string
	ImageURL   string
	FolderName string
	NumChapters int
	IsSingleCut bool
}

// mcChapter holds chapter metadata from the watch bundle.
type mcChapter struct {
	Title      string
	Desc       string
	MediaUUID  string
	SeqNumber  int
	ThumbURL   string
}

// masterclassHeaders returns common HTTP headers for MasterClass www API requests.
func (a *App) masterclassHeaders() map[string]string {
	return map[string]string{
		"User-Agent":     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
		"Accept":         "application/json",
		"Content-Type":   "application/json",
		"Cookie":         a.masterclass.cookies,
		"Referer":        mcBaseURL + "/library-search",
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	}
}

// masterclassEdgeHeaders returns headers for edge.masterclass.com API requests.
func (a *App) masterclassEdgeHeaders() map[string]string {
	return map[string]string{
		"User-Agent":     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
		"Accept":         "application/json",
		"Cookie":         a.masterclass.cookies,
		"X-Api-Key":      mcEdgeAPIKey,
		"Mc-Profile-Id":  a.masterclass.profileID,
		"Origin":         mcBaseURL,
		"Referer":        mcBaseURL + "/",
		"Sec-Fetch-Site": "same-site",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	}
}

// masterclassLogin validates MasterClass session cookies.
// The "email" field is repurposed for the cookie string.
func (a *App) masterclassLogin(email, password string) map[string]interface{} {
	raw := strings.TrimSpace(email)
	if raw == "" {
		return map[string]interface{}{
			"success": false,
			"error":   "Paste your MasterClass cookies from browser DevTools (F12 > Network > copy cookie header).",
		}
	}

	// Handle multi-line "cookie: value" format
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

	// Validate cookies by fetching watchlist
	hdrs := map[string]string{
		"User-Agent":     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
		"Accept":         "application/json",
		"Content-Type":   "application/json",
		"Cookie":         cookies,
		"Referer":        mcBaseURL + "/library-search",
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	}

	data, status, err := httpGetJSON(mcBaseURL+"/jsonapi/v1/watch-list-items?deep=true", hdrs)
	if err != nil {
		return map[string]interface{}{"success": false, "error": "Connection error: " + err.Error()}
	}
	if status != 200 {
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("Invalid cookies (HTTP %d). Make sure you are logged in.", status)}
	}

	// Verify it's a valid JSON array
	var items []interface{}
	if json.Unmarshal(data, &items) != nil {
		return map[string]interface{}{"success": false, "error": "Unexpected response format."}
	}

	// Extract profile ID from first item
	profileID := ""
	if len(items) > 0 {
		if item, ok := items[0].(map[string]interface{}); ok {
			if user, ok := item["user"].(map[string]interface{}); ok {
				if pid, ok := user["profile_id"].(float64); ok {
					profileID = fmt.Sprintf("%d", int(pid))
				}
			}
			if profileID == "" {
				if profile, ok := item["profile"].(map[string]interface{}); ok {
					if pid, ok := profile["id"].(float64); ok {
						profileID = fmt.Sprintf("%d", int(pid))
					}
				}
			}
		}
	}

	a.masterclass.cookies = cookies
	a.masterclass.profileID = profileID
	return map[string]interface{}{"success": true}
}

// masterclassGetCourses fetches the user's watchlist.
func (a *App) masterclassGetCourses() []map[string]interface{} {
	hdrs := a.masterclassHeaders()

	data, status, err := httpGetJSON(mcBaseURL+"/jsonapi/v1/watch-list-items?deep=true", hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var items []interface{}
	if json.Unmarshal(data, &items) != nil {
		return nil
	}

	// Extract profile ID if not yet set
	if a.masterclass.profileID == "" && len(items) > 0 {
		if item, ok := items[0].(map[string]interface{}); ok {
			if user, ok := item["user"].(map[string]interface{}); ok {
				if pid, ok := user["profile_id"].(float64); ok {
					a.masterclass.profileID = fmt.Sprintf("%d", int(pid))
				}
			}
		}
	}

	var courses []MasterClassCourse
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		listable, ok := item["listable"].(map[string]interface{})
		if !ok || listable == nil {
			continue
		}

		title := jsonStr(listable, "title")
		instructor := jsonStr(listable, "instructor_name")
		slug := jsonStr(listable, "slug")
		imageURL := jsonStr(listable, "class_tile_image")
		numChapters := 0
		if n, ok := listable["num_chapters"].(float64); ok {
			numChapters = int(n)
		}
		isSingleCut := false
		if v, ok := listable["is_single_cut"].(bool); ok {
			isSingleCut = v
		}
		courseID := 0
		if n, ok := listable["id"].(float64); ok {
			courseID = int(n)
		}

		folderName := sanitizeFilename(instructor + " - " + title)

		courses = append(courses, MasterClassCourse{
			ID:          courseID,
			Title:       title,
			Slug:        slug,
			Instructor:  instructor,
			ImageURL:    imageURL,
			FolderName:  folderName,
			NumChapters: numChapters,
			IsSingleCut: isSingleCut,
		})
	}

	a.masterclass.courses = courses

	result := make([]map[string]interface{}, len(courses))
	for i, c := range courses {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), c.FolderName))
		result[i] = map[string]interface{}{
			"name":        c.Instructor + " - " + c.Title,
			"preview_url": c.ImageURL,
			"downloaded":  dlStatus != "none",
			"dl_status":   dlStatus,
		}
	}
	return result
}

// masterclassGetUUID fetches the course UUID from its slug.
func (a *App) masterclassGetUUID(slug string) string {
	hdrs := a.masterclassHeaders()
	data, status, err := httpGetJSON(mcBaseURL+"/api/v3/unique-identifiers/courses/"+slug, hdrs)
	if err != nil || status != 200 {
		return ""
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) != nil {
		return ""
	}
	return jsonStr(resp, "uuid")
}

// masterclassGetWatchBundle fetches the watch bundle (chapters/contents) for a course.
func (a *App) masterclassGetWatchBundle(uuid string) map[string]interface{} {
	hdrs := a.masterclassHeaders()
	data, status, err := httpGetJSON(mcBaseURL+"/api/v3/watch-bundles/"+uuid, hdrs)
	if err != nil || status != 200 {
		return nil
	}
	var bundle map[string]interface{}
	if json.Unmarshal(data, &bundle) != nil {
		return nil
	}
	return bundle
}

// masterclassGetMediaMetadata fetches video sources and subtitles from the edge API.
func (a *App) masterclassGetMediaMetadata(mediaUUID string) map[string]interface{} {
	hdrs := a.masterclassEdgeHeaders()
	data, status, err := httpGetJSON(mcEdgeURL+"/api/v1/media/metadata/"+mediaUUID, hdrs)
	if err != nil || status != 200 {
		return nil
	}
	var meta map[string]interface{}
	if json.Unmarshal(data, &meta) != nil {
		return nil
	}
	return meta
}

// --- Download Orchestration ---

func (a *App) masterclassDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.masterclassDownloadOne(idx) {
			if a.cancel.Load() {
				break
			}
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}

func (a *App) masterclassDownloadOne(index int) bool {
	if index < 0 || index >= len(a.masterclass.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course."})
		return false
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ok, retry := a.masterclassDownloadOneAttempt(index)
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
				"percent":  0,
			})
			time.Sleep(time.Duration(3*attempt) * time.Second)
		}
	}
	return false
}

func (a *App) masterclassDownloadOneAttempt(index int) (ok bool, retryable bool) {
	defer func() {
		if r := recover(); r != nil {
			a.emit("dl_error", map[string]interface{}{
				"index": index, "message": fmt.Sprintf("Error: %v", r),
			})
			ok = false
			retryable = true
		}
	}()

	course := a.masterclass.courses[index]
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

	// Get UUID
	uuid := a.masterclassGetUUID(course.Slug)
	if uuid == "" {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Failed to get course UUID",
		})
		return false, true
	}

	// Get watch bundle
	bundle := a.masterclassGetWatchBundle(uuid)
	if bundle == nil {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Failed to fetch course contents",
		})
		return false, true
	}

	// Save course info JSON
	infoPath := filepath.Join(courseDir, "course_info.json")
	if _, err := os.Stat(infoPath); os.IsNotExist(err) {
		if infoData, err := json.MarshalIndent(bundle, "", "  "); err == nil {
			os.WriteFile(infoPath, infoData, 0644)
		}
	}

	// Save description
	if text, ok := bundle["text"].(map[string]interface{}); ok {
		desc := jsonStr(text, "description")
		if desc != "" {
			descPath := filepath.Join(courseDir, "description.txt")
			if _, err := os.Stat(descPath); os.IsNotExist(err) {
				os.WriteFile(descPath, []byte(course.Title+"\n"+strings.Repeat("=", len(course.Title))+"\n\n"+desc+"\n"), 0644)
			}
		}
	}

	// Download cover image
	if img, ok := bundle["image"].(map[string]interface{}); ok {
		if bg, ok := img["background"].(map[string]interface{}); ok {
			coverURL := ""
			if r16x9, ok := bg["16x9"].(map[string]interface{}); ok {
				coverURL = jsonStr(r16x9, "url")
			}
			if coverURL == "" {
				if r12x5, ok := bg["12x5"].(map[string]interface{}); ok {
					coverURL = jsonStr(r12x5, "url")
				}
			}
			if coverURL != "" {
				coverPath := filepath.Join(courseDir, "cover.jpg")
				if _, err := os.Stat(coverPath); os.IsNotExist(err) {
					downloadToFileSimple(coverURL, coverPath, nil)
				}
			}
		}
	}

	// Parse chapters
	contents := jsonArray(bundle, "contents")
	var chapters []mcChapter
	for i, raw := range contents {
		cm, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		text, _ := cm["text"].(map[string]interface{})
		title := ""
		desc := ""
		if text != nil {
			title = jsonStr(text, "title")
			desc = jsonStr(text, "description")
		}
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}

		mediaUUID := jsonStr(cm, "media_uuid")
		seqNum := i + 1
		if n, ok := cm["sequence_number"].(float64); ok {
			seqNum = int(n)
		}

		thumbURL := ""
		if img, ok := cm["image"].(map[string]interface{}); ok {
			if thumb, ok := img["thumbnail"].(map[string]interface{}); ok {
				if r16x9, ok := thumb["16x9"].(map[string]interface{}); ok {
					thumbURL = jsonStr(r16x9, "url")
				}
			}
		}

		chapters = append(chapters, mcChapter{
			Title:     title,
			Desc:      desc,
			MediaUUID: mediaUUID,
			SeqNumber: seqNum,
			ThumbURL:  thumbURL,
		})
	}

	totalItems := len(chapters) + 1 // +1 for cover
	currentItem := 1                // cover already done

	// Download chapters
	for _, ch := range chapters {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false, false
		}

		currentItem++
		safeName := sanitizeFilename(fmt.Sprintf("%02d - %s", ch.SeqNumber, ch.Title))
		chapterDir := filepath.Join(courseDir, safeName)
		os.MkdirAll(chapterDir, 0755)

		// Save description
		if ch.Desc != "" {
			descPath := filepath.Join(chapterDir, "description.txt")
			if _, err := os.Stat(descPath); os.IsNotExist(err) {
				os.WriteFile(descPath, []byte(ch.Title+"\n"+strings.Repeat("=", len(ch.Title))+"\n\n"+ch.Desc+"\n"), 0644)
			}
		}

		// Download thumbnail
		if ch.ThumbURL != "" {
			thumbPath := filepath.Join(chapterDir, "thumbnail.jpg")
			if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
				downloadToFileSimple(ch.ThumbURL, thumbPath, nil)
			}
		}

		if ch.MediaUUID == "" {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentItem, "total": totalItems,
				"filename": ch.Title + " (no media)", "percent": 100,
			})
			continue
		}

		// Check if video already exists
		videoPath := filepath.Join(chapterDir, "video.mp4")
		if _, err := os.Stat(videoPath); err == nil {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentItem, "total": totalItems,
				"filename": ch.Title + " (already downloaded)", "percent": 100,
			})
			// Still download subtitles if missing
			a.masterclassDownloadSubtitles(ch.MediaUUID, chapterDir)
			continue
		}

		// Get media metadata from edge API
		meta := a.masterclassGetMediaMetadata(ch.MediaUUID)
		if meta == nil {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentItem, "total": totalItems,
				"filename": ch.Title + " (metadata failed)", "percent": 0,
			})
			continue
		}

		// Find m3u8 source
		m3u8URL := ""
		sources := jsonArray(meta, "sources")
		for _, s := range sources {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			if jsonStr(sm, "type") == "application/x-mpegURL" {
				m3u8URL = jsonStr(sm, "src")
				break
			}
		}

		if m3u8URL == "" {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentItem, "total": totalItems,
				"filename": ch.Title + " (no HLS source)", "percent": 0,
			})
			continue
		}

		// Download video via yt-dlp
		a.masterclassDownloadVideo(m3u8URL, videoPath, index, currentItem, totalItems, ch.Title)

		// Download subtitles
		a.masterclassDownloadSubtitlesFromMeta(meta, chapterDir)
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseDir})
	return true, false
}

// masterclassDownloadVideo downloads an HLS video via yt-dlp.
func (a *App) masterclassDownloadVideo(m3u8URL, outputPath string, index, current, total int, filename string) {
	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": filename, "percent": 0,
	})

	progress := func(dl, tb int64) {
		pct := 50.0
		if tb > 0 {
			pct = float64(dl) / float64(tb) * 100.0
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": filename, "percent": pct,
		})
	}

	args := []string{
		"--no-check-certificates",
		"--progress", "--newline",
		"-S", "height:720",
		"--concurrent-fragments", "8",
		"--merge-output-format", "mp4",
		"-o", outputPath,
		m3u8URL,
	}

	err := a.runYtdlpArgs(args, progress)
	if err != nil {
		// Clean up incomplete/temp files
		cleanYtdlpTempFiles(outputPath)
		if a.cancel.Load() || strings.Contains(err.Error(), "cancelled") {
			return
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": filename + " (FAILED: " + err.Error() + ")", "percent": 0,
		})
		return
	}

	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": filename, "percent": 100,
	})
}

// masterclassDownloadSubtitles fetches media metadata and downloads subtitles.
func (a *App) masterclassDownloadSubtitles(mediaUUID, chapterDir string) {
	meta := a.masterclassGetMediaMetadata(mediaUUID)
	if meta == nil {
		return
	}
	a.masterclassDownloadSubtitlesFromMeta(meta, chapterDir)
}

// masterclassDownloadSubtitlesFromMeta downloads subtitles from pre-fetched metadata.
func (a *App) masterclassDownloadSubtitlesFromMeta(meta map[string]interface{}, chapterDir string) {
	tracks := jsonArray(meta, "text_tracks")
	for _, t := range tracks {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		lang := jsonStr(tm, "srclang")
		src := jsonStr(tm, "src")
		if lang == "" || src == "" {
			continue
		}
		vttPath := filepath.Join(chapterDir, "subtitles_"+lang+".vtt")
		if _, err := os.Stat(vttPath); os.IsNotExist(err) {
			downloadToFileSimple(src, vttPath, nil)
		}
	}
}
