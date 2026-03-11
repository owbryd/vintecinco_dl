package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Kiwify API constants.
// Authentication goes through Google Identity Toolkit (Firebase Auth).
// Course data is fetched from Kiwify's admin API. Club content (community
// features bundled with courses) uses a separate API base.
const (
	kiwifyAuthKey      = "AIzaSyDmOO1YAGt0X35zykOMTlolvsoBkefLKFU"
	kiwifyAuthEndpoint = "https://www.googleapis.com/identitytoolkit/v3/relyingparty/verifyPassword"
	kiwifyBaseAPI      = "https://admin-api.kiwify.com.br/v1/viewer"
	kiwifyClubsAPI     = "https://admin-api.kiwify.com/v1/viewer"
	kiwifyUA           = "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0"
)

// KiwifyState holds the authentication token and cached course list for Kiwify.
type KiwifyState struct {
	token   string
	courses []KiwifyCourse
}

// KiwifyCourse represents a single purchased course on Kiwify.
type KiwifyCourse struct {
	Raw          map[string]interface{} // original JSON from the API
	ID           string
	Name         string
	PurchaseDate string
	ImageURL     string
	FolderName   string // sanitized folder name on disk, e.g. "[15-06-2024] Course Name"
}

// kiwifyLogin authenticates with Kiwify via Firebase Auth (email/password).
// On success, stores the ID token for subsequent API calls.
func (a *App) kiwifyLogin(email, password string) map[string]interface{} {
	body, _ := json.Marshal(map[string]interface{}{
		"email":             email,
		"password":          password,
		"returnSecureToken": true,
	})

	url := kiwifyAuthEndpoint + "?key=" + kiwifyAuthKey
	resp, err := httpRequest("POST", url, bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"}, true)
	if err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if token, ok := result["idToken"].(string); ok && resp.StatusCode == 200 {
		a.kiwify.token = token
		return map[string]interface{}{"success": true}
	}
	return map[string]interface{}{"success": false, "error": "Authentication failed"}
}

// kiwifyAPIHeaders returns HTTP headers for Kiwify API requests,
// including the Bearer token and browser-like User-Agent.
func (a *App) kiwifyAPIHeaders() map[string]string {
	return map[string]string{
		"Authorization":  "Bearer " + a.kiwify.token,
		"User-Agent":     kiwifyUA,
		"Accept":         "application/json, text/plain, */*",
		"Accept-Language": "pt-BR,pt;q=0.9",
		"Origin":         "https://dashboard.kiwify.com",
		"Referer":        "https://dashboard.kiwify.com/",
	}
}

// kiwifyDownloadHeaders returns HTTP headers for downloading Kiwify files.
// These use the .com.br domain as referer (required by the CDN).
func (a *App) kiwifyDownloadHeaders() map[string]string {
	return map[string]string{
		"User-Agent":     kiwifyUA,
		"Accept":         "*/*",
		"Accept-Language": "pt-BR,pt;q=0.8,en-US;q=0.5,en;q=0.3",
		"Referer":        "https://dashboard.kiwify.com.br/",
	}
}

// kiwifyName extracts a display name from a Kiwify JSON object by trying
// multiple field names in priority order: name > title > label > description.
func kiwifyName(m map[string]interface{}) string {
	for _, key := range []string{"name", "title", "label", "description"} {
		if v := jsonStr(m, key); v != "" {
			return v
		}
	}
	return ""
}

// parseKiwifyCourse extracts course metadata from an API response map.
// Handles two response structures: nested "course_info" or flat top-level fields.
// Image URL resolution follows a priority chain: course_img > image > thumbnail.
func parseKiwifyCourse(raw map[string]interface{}) KiwifyCourse {
	kc := KiwifyCourse{Raw: raw}

	// Try nested course_info first (primary structure)
	if ci := jsonMap(raw, "course_info"); ci != nil {
		kc.ID = jsonStr(ci, "id")
		kc.Name = jsonStr(ci, "name")
		if img := jsonStr(ci, "course_img"); img != "" {
			kc.ImageURL = img
		} else if img := jsonStr(ci, "image"); img != "" {
			kc.ImageURL = img
		} else if img := jsonStr(ci, "thumbnail"); img != "" {
			kc.ImageURL = img
		}
	}

	// Fallback to top-level fields
	if kc.ID == "" {
		kc.ID = jsonStr(raw, "id")
	}
	if kc.Name == "" {
		kc.Name = jsonStr(raw, "name")
		if kc.Name == "" {
			kc.Name = "Unnamed Course"
		}
	}
	if kc.ImageURL == "" {
		if img := jsonStr(raw, "course_img"); img != "" {
			kc.ImageURL = img
		} else if img := jsonStr(raw, "image"); img != "" {
			kc.ImageURL = img
		} else if img := jsonStr(raw, "thumbnail"); img != "" {
			kc.ImageURL = img
		}
	}

	kc.PurchaseDate = jsonStr(raw, "purchase_date")
	kc.FolderName = makeCourseFolder(kc.Name, kc.PurchaseDate)
	return kc
}

// kiwifyGetCourses fetches all purchased courses from the Kiwify API.
// It paginates through the course list and falls back to alternative
// endpoints if few courses are found (some accounts have different structures).
func (a *App) kiwifyGetCourses() []map[string]interface{} {
	hdrs := a.kiwifyAPIHeaders()
	var allCourses []KiwifyCourse

	// Paginate through courses (20 per page by default)
	for page := 1; page <= 100; page++ {
		url := fmt.Sprintf("%s/schools/courses?page=%d&archived=false", kiwifyBaseAPI, page)
		data, status, err := httpGetJSON(url, hdrs)
		if err != nil || status != 200 {
			break
		}

		var parsed map[string]interface{}
		if json.Unmarshal(data, &parsed) != nil {
			break
		}

		courses := jsonArray(parsed, "courses")
		if len(courses) == 0 {
			break
		}

		for _, c := range courses {
			if cm, ok := c.(map[string]interface{}); ok {
				kc := parseKiwifyCourse(cm)
				if kc.ID != "" {
					allCourses = append(allCourses, kc)
				}
			}
		}

		// Less than 20 means we've reached the last page
		if len(courses) < 20 {
			break
		}
	}

	// Fallback: if few courses found, try endpoints without pagination
	// (some Kiwify accounts return all courses in a single response)
	if len(allCourses) <= 10 {
		for _, altURL := range []string{
			kiwifyBaseAPI + "/schools/courses?archived=false&limit=1000",
			kiwifyBaseAPI + "/schools/courses?archived=false",
		} {
			data, _, err := httpGetJSON(altURL, hdrs)
			if err != nil {
				continue
			}
			var parsed map[string]interface{}
			if json.Unmarshal(data, &parsed) != nil {
				continue
			}
			courses := jsonArray(parsed, "courses")
			if len(courses) > len(allCourses) {
				allCourses = nil
				for _, c := range courses {
					if cm, ok := c.(map[string]interface{}); ok {
						kc := parseKiwifyCourse(cm)
						if kc.ID != "" {
							allCourses = append(allCourses, kc)
						}
					}
				}
				break
			}
		}
	}

	a.kiwify.courses = allCourses

	// Build frontend-friendly response
	result := make([]map[string]interface{}, len(allCourses))
	for i, c := range allCourses {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), c.FolderName))
		result[i] = map[string]interface{}{
			"name":        c.Name,
			"date":        formatPurchaseDate(c.PurchaseDate),
			"preview_url": c.ImageURL,
			"downloaded":  dlStatus != "none",
			"dl_status":   dlStatus,
		}
	}
	return result
}

// --- Club Detection ---
//
// Some Kiwify courses are "clubs" — a wrapper that contains one or more
// actual courses inside. The public course ID from the purchase may not
// point directly to downloadable content; instead, we need to discover
// the real course ID(s) inside the club.

// kiwifyFindRealCourseID resolves a public course ID to the actual
// content ID. First checks if the ID already has sections/modules.
// If not, queries club API endpoints to find nested course IDs.
func (a *App) kiwifyFindRealCourseID(publicID string) string {
	hdrs := a.kiwifyAPIHeaders()

	// Method 1: Check if public ID already has downloadable content
	data, status, err := httpGetJSON(kiwifyBaseAPI+"/courses/"+publicID+"/sections", hdrs)
	if err == nil && status == 200 {
		var parsed map[string]interface{}
		if json.Unmarshal(data, &parsed) == nil {
			if courseData := jsonMap(parsed, "course"); courseData != nil {
				modules := jsonArray(courseData, "modules")
				sections := jsonArray(courseData, "sections")
				if len(modules) > 0 || len(sections) > 0 {
					return publicID
				}
			}
		}
	}

	// Method 2: Try clubs API endpoints to find embedded course IDs
	clubHeaders := map[string]string{
		"User-Agent":    kiwifyUA,
		"Accept":        "application/json, text/plain, */*",
		"Authorization": "Bearer " + a.kiwify.token,
		"Origin":        "https://members.kiwify.com",
	}

	endpoints := []string{
		kiwifyClubsAPI + "/clubs/" + publicID + "/content?caipirinha=true",
		kiwifyClubsAPI + "/clubs/" + publicID + "/content",
		kiwifyClubsAPI + "/clubs?clubId=" + publicID,
	}

	for _, ep := range endpoints {
		data, status, err := httpGetJSON(ep, clubHeaders)
		if err != nil || status != 200 {
			continue
		}
		var parsed map[string]interface{}
		if json.Unmarshal(data, &parsed) != nil {
			continue
		}

		var ids []string

		// The club API can return course IDs in several different structures
		// depending on the club type. We try all known patterns.

		// Structure 1: { "data": { "content": { "sections": [...] } } }
		if d := jsonMap(parsed, "data"); d != nil {
			if content := jsonMap(d, "content"); content != nil {
				for _, sec := range jsonArray(content, "sections") {
					sm, _ := sec.(map[string]interface{})
					if sm == nil {
						continue
					}
					stype := jsonStr(sm, "type")
					if stype == "courses" {
						for _, item := range jsonArray(sm, "items") {
							im, _ := item.(map[string]interface{})
							if id := jsonStr(im, "id"); id != "" {
								ids = append(ids, id)
							}
						}
					} else if stype == "modules" {
						for _, item := range jsonArray(sm, "items") {
							im, _ := item.(map[string]interface{})
							if id := jsonStr(im, "course_id"); id != "" {
								ids = append(ids, id)
							}
						}
					}
				}
			}
			for _, c := range jsonArray(d, "courses") {
				cm, _ := c.(map[string]interface{})
				if id := jsonStr(cm, "id"); id != "" {
					ids = append(ids, id)
				}
			}
			if allCourses := jsonMap(d, "all_courses"); allCourses != nil {
				for k := range allCourses {
					ids = append(ids, k)
				}
			}
		}

		// Structure 2: { "courses": [...] } at root
		for _, c := range jsonArray(parsed, "courses") {
			cm, _ := c.(map[string]interface{})
			if id := jsonStr(cm, "id"); id != "" {
				ids = append(ids, id)
			}
		}

		// Structure 3: { "club": { "courses": [...] } }
		if club := jsonMap(parsed, "club"); club != nil {
			for _, c := range jsonArray(club, "courses") {
				cm, _ := c.(map[string]interface{})
				if id := jsonStr(cm, "id"); id != "" {
					ids = append(ids, id)
				}
			}
		}

		// Structure 4: { "pages": [...] }
		for _, page := range jsonArray(parsed, "pages") {
			pm, _ := page.(map[string]interface{})
			if pm == nil {
				continue
			}
			for _, c := range jsonArray(pm, "courses") {
				cm, _ := c.(map[string]interface{})
				if id := jsonStr(cm, "id"); id != "" {
					ids = append(ids, id)
				}
			}
			for _, m := range jsonArray(pm, "modules") {
				mm, _ := m.(map[string]interface{})
				if id := jsonStr(mm, "course_id"); id != "" {
					ids = append(ids, id)
				}
			}
		}

		// Deduplicate and return first found ID
		seen := map[string]bool{}
		var unique []string
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				unique = append(unique, id)
			}
		}

		if len(unique) > 0 {
			return unique[0]
		}
	}

	return publicID
}

// --- File Download ---
//
// Kiwify stores files behind multiple layers of indirection. A file may
// be accessible via direct URL, redirect URL, lesson-specific endpoint, or
// token-authenticated fallback. downloadKiwifyFile tries 5 methods in order.

// downloadKiwifyFile attempts to download a single file attachment using
// multiple API methods. Returns true if the file was saved successfully.
//
// Methods tried (in order):
//  1. API redirect endpoint (forceDownload=true, capture 3xx Location header)
//  2. Direct URL from the file object
//  3. Lesson-specific file endpoint
//  4. Lesson detail API to find URL by file ID
//  5. Fallback URLs with authentication token
func (a *App) downloadKiwifyFile(courseID string, file map[string]interface{},
	saveDir, lessonID string) bool {

	fileName := jsonStr(file, "name")
	if fileName == "" {
		fileName = "unknown"
	}
	fileID := jsonStr(file, "id")
	fileURLDirect := jsonStr(file, "url")

	if fileID == "" && fileURLDirect == "" {
		return false
	}

	savePath := filepath.Join(saveDir, sanitizeFilename(fileName))
	if _, err := os.Stat(savePath); err == nil {
		return true // Already downloaded
	}
	os.MkdirAll(saveDir, 0755)
	hdrs := a.kiwifyAPIHeaders()
	dlHdrs := a.kiwifyDownloadHeaders()

	// Method 1: API redirect — request with forceDownload, capture redirect URL
	if fileID != "" {
		url := kiwifyBaseAPI + "/courses/" + courseID + "/files/" + fileID + "?forceDownload=true"
		resp, err := httpRequest("GET", url, nil, hdrs, false)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 301 && resp.StatusCode <= 308 {
				loc := resp.Header.Get("Location")
				if loc != "" {
					if downloadToFileSimple(loc, savePath, dlHdrs) == nil {
						return true
					}
				}
			}
			if resp.StatusCode == 200 {
				body, _ := io.ReadAll(resp.Body)
				var jdata map[string]interface{}
				if json.Unmarshal(body, &jdata) == nil {
					if u, ok := jdata["url"].(string); ok && u != "" {
						if downloadToFileSimple(u, savePath, dlHdrs) == nil {
							return true
						}
					}
				}
				// If response is large enough, it might be the file content itself
				if len(body) > 100 {
					os.MkdirAll(filepath.Dir(savePath), 0755)
					if os.WriteFile(savePath, body, 0644) == nil {
						return true
					}
				}
			}
		}
	}

	// Method 2: Direct URL from the file object
	if fileURLDirect != "" {
		if downloadToFileSimple(fileURLDirect, savePath, dlHdrs) == nil {
			return true
		}
	}

	if fileID == "" {
		return false
	}

	// Method 3: Lesson-specific file endpoint
	if lessonID != "" {
		url := kiwifyBaseAPI + "/courses/" + courseID + "/lesson/" + lessonID + "/files/" + fileID
		if downloadToFileSimple(url, savePath, hdrs) == nil {
			return true
		}
	}

	// Method 4: Fetch lesson details to find the file URL by ID
	if lessonID != "" {
		url := kiwifyBaseAPI + "/courses/" + courseID + "/lesson/" + lessonID
		data, _, err := httpGetJSON(url, hdrs)
		if err == nil {
			var jdata map[string]interface{}
			if json.Unmarshal(data, &jdata) == nil {
				lessonData := jdata
				if ld := jsonMap(jdata, "lesson"); ld != nil {
					lessonData = ld
				} else if ld := jsonMap(jdata, "data"); ld != nil {
					lessonData = ld
				}
				for _, f := range jsonArray(lessonData, "files") {
					fm, _ := f.(map[string]interface{})
					if jsonStr(fm, "id") == fileID {
						if u := jsonStr(fm, "url"); u != "" {
							if downloadToFileSimple(u, savePath, dlHdrs) == nil {
								return true
							}
						}
					}
				}
			}
		}
	}

	// Method 5: Last resort — token-authenticated URLs
	fallbacks := []string{
		kiwifyBaseAPI + "/courses/" + courseID + "/files/" + fileID + "/download?token=" + a.kiwify.token,
		kiwifyBaseAPI + "/courses/" + courseID + "/downloadable/" + fileID + "?token=" + a.kiwify.token,
		"https://admin-api.kiwify.com.br/v1/files/" + fileID + "/download?token=" + a.kiwify.token,
	}
	for _, furl := range fallbacks {
		if downloadToFileSimple(furl, savePath, hdrs) == nil {
			return true
		}
	}

	return false
}

// --- Lesson Content Download ---

// linkRe matches <a href="..."> links pointing to downloadable files
// (pdf, docx, xlsx, zip, mp3, etc.) inside lesson HTML content.
var linkRe = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+\.(?:pdf|docx?|xlsx?|xls|pptx?|txt|csv|zip|rar|epub|mp3|odt|ods)(?:\?[^"']*)?)["']`)

// kiwifyDownloadLessonContent downloads all content for a single lesson:
//   - HTML description (saved as description.html)
//   - Video (via yt-dlp, from YouTube/stream URLs)
//   - Attachments (PDFs, docs, etc. from multiple API fields and HTML links)
func (a *App) kiwifyDownloadLessonContent(courseID string, lesson map[string]interface{},
	lessonPath string, index, current, total int) {

	lessonID := jsonStr(lesson, "id")
	if lessonID == "" {
		return
	}

	// Fetch detailed lesson data from API
	hdrs := a.kiwifyAPIHeaders()
	data, _, err := httpGetJSON(kiwifyBaseAPI+"/courses/"+courseID+"/lesson/"+lessonID, hdrs)
	if err != nil {
		return
	}

	var raw map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		return
	}

	// Unwrap common API response wrappers ("lesson" or "data" keys)
	details := raw
	if ld := jsonMap(raw, "lesson"); ld != nil {
		details = ld
	} else if ld := jsonMap(raw, "data"); ld != nil {
		details = ld
	}

	// Merge API details with the original lesson summary
	merged := make(map[string]interface{})
	for k, v := range lesson {
		merged[k] = v
	}
	for k, v := range details {
		merged[k] = v
	}

	lessonName := jsonStr(merged, "title")
	if lessonName == "" {
		lessonName = "Lesson"
	}

	progress := func(dl, tb int64) {
		pct := 50.0
		if tb > 0 {
			pct = float64(dl) / float64(tb) * 100.0
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": lessonName, "percent": pct,
		})
	}

	// Save HTML description
	description := jsonStr(merged, "content")
	if description != "" {
		descPath := filepath.Join(lessonPath, "description.html")
		if _, err := os.Stat(descPath); os.IsNotExist(err) {
			os.WriteFile(descPath, []byte(description), 0644)
		}
	}

	// --- Find video URL ---
	// Kiwify stores videos in many different fields depending on the course type.
	// We check all known locations in priority order.
	youtubeVideo := jsonStr(merged, "youtube_video")
	var streamLink string
	if videoObj := jsonMap(merged, "video"); videoObj != nil {
		for _, key := range []string{"stream_link", "url", "hls_url", "dash_url", "source", "player_url"} {
			if v := jsonStr(videoObj, key); v != "" {
				streamLink = v
				break
			}
		}
	} else if v, ok := merged["video"].(string); ok {
		streamLink = v
	}

	// Try additional top-level video fields
	if streamLink == "" {
		for _, key := range []string{"video_url", "media_url", "stream_url", "hls_url",
			"player_url", "embed_url", "media", "video_link"} {
			if v := jsonStr(merged, key); v != "" {
				streamLink = v
				break
			}
		}
	}

	// Determine final video URL (prefer YouTube, then stream link)
	var videoURL string
	if youtubeVideo != "" &&
		(strings.Contains(youtubeVideo, "youtube.com") || strings.Contains(youtubeVideo, "youtu.be")) {
		videoURL = youtubeVideo
	} else if streamLink != "" {
		if strings.HasPrefix(streamLink, "http") {
			videoURL = streamLink
		} else {
			videoURL = "https://" + streamLink
		}
	}

	// Download video via yt-dlp
	if videoURL != "" && !videoExists(lessonPath) {
		outTpl := filepath.Join(lessonPath, "video.%(ext)s")
		if err := a.runYtdlp(outTpl, videoURL, progress); err != nil {
			cleanYtdlpTempFiles(outTpl)
			if !a.cancel.Load() {
				a.emit("dl_progress", map[string]interface{}{
					"index": index, "current": current, "total": total,
					"filename": lessonName + " (video failed: " + err.Error() + ")", "percent": 0,
				})
			}
		}
	}

	// --- Download attachments ---
	// Collect files from all possible API fields
	var filesArr []map[string]interface{}

	// Array fields that may contain file lists
	for _, field := range []string{"files", "attachments", "downloads", "materials",
		"resources", "complementary_materials", "downloadable_files"} {
		for _, f := range jsonArray(merged, field) {
			if fm, ok := f.(map[string]interface{}); ok {
				filesArr = append(filesArr, fm)
			}
		}
	}

	// Single-object fields
	for _, field := range []string{"file", "pdf", "document"} {
		if fm := jsonMap(merged, field); fm != nil {
			filesArr = append(filesArr, fm)
		}
	}

	// Direct URL string fields
	for _, field := range []string{"file_url", "pdf_url", "document_url", "download_url"} {
		if u := jsonStr(merged, field); u != "" && strings.HasPrefix(u, "http") {
			fname := extractFilenameFromURL(u)
			if fname == "" {
				fname = field + "_file"
			}
			filesArr = append(filesArr, map[string]interface{}{
				"name": fname, "url": u, "id": "",
			})
		}
	}

	// Extract downloadable links from HTML description content
	if description != "" {
		matches := linkRe.FindAllStringSubmatch(description, -1)
		for _, m := range matches {
			u := m[1]
			if !strings.HasPrefix(u, "http") {
				continue
			}
			fname := extractFilenameFromURL(u)
			if fname != "" {
				filesArr = append(filesArr, map[string]interface{}{
					"name": fname, "url": u, "id": "",
				})
			}
		}
	}

	// Download all collected attachments
	if len(filesArr) > 0 {
		attachDir := filepath.Join(lessonPath, "Attachments")
		downloadedNames := map[string]bool{} // deduplicate by filename

		for _, file := range filesArr {
			if a.cancel.Load() {
				return
			}
			fname := jsonStr(file, "name")
			if fname == "" {
				fname = "unknown"
			}
			if downloadedNames[fname] {
				continue
			}
			downloadedNames[fname] = true

			fid := jsonStr(file, "id")
			furl := jsonStr(file, "url")

			if fid != "" {
				// Use multi-method download with retries
				for attempt := 1; attempt <= 3; attempt++ {
					if a.downloadKiwifyFile(courseID, file, attachDir, lessonID) {
						break
					}
					if attempt < 3 {
						time.Sleep(time.Duration(2*attempt) * time.Second)
					}
				}
			} else if furl != "" {
				// Direct URL download with retries
				savePath := filepath.Join(attachDir, sanitizeFilename(fname))
				if _, err := os.Stat(savePath); os.IsNotExist(err) {
					os.MkdirAll(attachDir, 0755)
					for attempt := 1; attempt <= 3; attempt++ {
						if downloadToFileSimple(furl, savePath, a.kiwifyDownloadHeaders()) == nil {
							break
						}
						if attempt < 3 {
							time.Sleep(time.Duration(2*attempt) * time.Second)
						}
					}
				}
			}
		}
	}
}

// --- Course Download Orchestration ---

// kiwifyDownloadOne downloads a single course with up to 3 retry attempts.
// Returns true if the course was downloaded successfully.
func (a *App) kiwifyDownloadOne(index int) bool {
	if index < 0 || index >= len(a.kiwify.courses) {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Invalid course.",
		})
		return false
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ok, retry := a.kiwifyDownloadOneAttempt(index)
		if ok {
			return true
		}
		if !retry {
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

// kiwifyDownloadOneAttempt performs a single download attempt for a course.
// It fetches the course structure, iterates through sections/modules/lessons,
// and downloads each lesson's content. Panics are recovered and reported as
// retryable errors. Returns (ok, retryable).
func (a *App) kiwifyDownloadOneAttempt(index int) (ok bool, retryable bool) {
	defer func() {
		if r := recover(); r != nil {
			a.emit("dl_error", map[string]interface{}{
				"index": index, "message": fmt.Sprintf("Panic: %v", r),
			})
			ok = false
			retryable = true
		}
	}()

	course := a.kiwify.courses[index]
	courseID := course.ID

	if courseID == "" {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Course has no ID.",
		})
		return false, false
	}

	courseDir := filepath.Join(a.platformDir(), course.FolderName)
	os.MkdirAll(courseDir, 0755)

	a.emit("dl_started", map[string]interface{}{
		"index": index, "folder": courseDir,
	})

	// Resolve the real course ID (handles club-wrapped courses)
	realID := a.kiwifyFindRealCourseID(courseID)

	// Fetch course structure (sections + modules + lessons)
	hdrs := a.kiwifyAPIHeaders()
	data, _, err := httpGetJSON(kiwifyBaseAPI+"/courses/"+realID+"/sections", hdrs)
	if err != nil {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Failed to retrieve course data.",
		})
		return false, true
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil || jsonMap(parsed, "course") == nil {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Failed to parse course data.",
		})
		return false, true
	}

	courseData := jsonMap(parsed, "course")
	sections := jsonArray(courseData, "sections")
	modules := jsonArray(courseData, "modules")

	if len(sections) == 0 && len(modules) == 0 {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Course has no content.",
		})
		return false, false
	}

	// Count total lessons for progress display
	totalLessons := 0
	if len(sections) > 0 {
		for _, s := range sections {
			sm, _ := s.(map[string]interface{})
			for _, m := range jsonArray(sm, "modules") {
				mm, _ := m.(map[string]interface{})
				totalLessons += len(jsonArray(mm, "lessons"))
			}
		}
	} else {
		for _, m := range modules {
			mm, _ := m.(map[string]interface{})
			totalLessons += len(jsonArray(mm, "lessons"))
		}
	}

	// Clean up leftover .part files from previous interrupted downloads
	filepath.Walk(courseDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".part" {
			os.Remove(path)
		}
		return nil
	})

	// Download course-level materials (files attached to the course itself)
	for _, cf := range jsonArray(courseData, "files") {
		if a.cancel.Load() {
			break
		}
		if cfm, ok := cf.(map[string]interface{}); ok {
			matDir := filepath.Join(courseDir, "Course_Materials")
			for att := 1; att <= 3; att++ {
				if a.downloadKiwifyFile(realID, cfm, matDir, "") {
					break
				}
				if att < 3 {
					time.Sleep(time.Duration(2*att) * time.Second)
				}
			}
		}
	}

	// --- Download all lessons ---
	currentLesson := 0

	// processLesson downloads a single lesson's content.
	// Returns false if cancelled, true otherwise.
	processLesson := func(lesson map[string]interface{}, lpath string) bool {
		if a.cancel.Load() {
			return false
		}

		currentLesson++
		lname := jsonStr(lesson, "title")
		if lname == "" {
			lname = fmt.Sprintf("Lesson %d", currentLesson)
		}

		if lessonAlreadyDone(lpath) {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentLesson, "total": totalLessons,
				"filename": lname + " (already downloaded)", "percent": 100,
			})
			return true
		}

		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": currentLesson, "total": totalLessons,
			"filename": lname, "percent": 0,
		})

		os.MkdirAll(lpath, 0755)
		// Recover from panics in individual lesson downloads
		// so a single broken lesson doesn't stop the entire course
		func() {
			defer func() { recover() }()
			a.kiwifyDownloadLessonContent(realID, lesson, lpath, index, currentLesson, totalLessons)
		}()
		return true
	}

	// downloadModuleFiles downloads files attached to a module (not lesson-level).
	downloadModuleFiles := func(mod map[string]interface{}, modDir string) {
		for _, field := range []string{"files", "attachments", "downloads", "materials", "resources"} {
			for _, mf := range jsonArray(mod, field) {
				if a.cancel.Load() {
					return
				}
				if mfm, ok := mf.(map[string]interface{}); ok {
					matDir := filepath.Join(modDir, "Module_Materials")
					for att := 1; att <= 3; att++ {
						if a.downloadKiwifyFile(realID, mfm, matDir, "") {
							break
						}
						if att < 3 {
							time.Sleep(time.Duration(2*att) * time.Second)
						}
					}
				}
			}
		}
	}

	// Iterate through the course hierarchy.
	// Kiwify courses have two possible structures:
	//   1. Sections > Modules > Lessons (3 levels)
	//   2. Modules > Lessons (2 levels, no sections)
	if len(sections) > 0 {
		for si, s := range sections {
			sm, _ := s.(map[string]interface{})
			sname := sanitizeFilename(kiwifyName(sm))
			if sname == "" {
				sname = "Section"
			}
			sectionDir := filepath.Join(courseDir, fmt.Sprintf("%02d - %s", si+1, sname))

			for mi, mod := range jsonArray(sm, "modules") {
				mm, _ := mod.(map[string]interface{})
				mname := sanitizeFilename(kiwifyName(mm))
				if mname == "" {
					mname = "Module"
				}
				modDir := filepath.Join(sectionDir, fmt.Sprintf("%02d - %s", mi+1, mname))

				downloadModuleFiles(mm, modDir)

				for li, lesson := range jsonArray(mm, "lessons") {
					lm, _ := lesson.(map[string]interface{})
					ltitle := sanitizeFilename(jsonStr(lm, "title"))
					if ltitle == "" {
						ltitle = "Lesson"
					}
					lpath := filepath.Join(modDir, fmt.Sprintf("%02d - %s", li+1, ltitle))
					if !processLesson(lm, lpath) {
						a.emit("dl_cancelled", map[string]interface{}{"index": index})
						return false, false
					}
				}
			}
		}
	} else {
		for mi, mod := range modules {
			mm, _ := mod.(map[string]interface{})
			mname := sanitizeFilename(kiwifyName(mm))
			if mname == "" {
				mname = "Module"
			}
			modDir := filepath.Join(courseDir, fmt.Sprintf("%02d - %s", mi+1, mname))

			downloadModuleFiles(mm, modDir)

			for li, lesson := range jsonArray(mm, "lessons") {
				lm, _ := lesson.(map[string]interface{})
				ltitle := sanitizeFilename(jsonStr(lm, "title"))
				if ltitle == "" {
					ltitle = "Lesson"
				}
				lpath := filepath.Join(modDir, fmt.Sprintf("%02d - %s", li+1, ltitle))
				if !processLesson(lm, lpath) {
					a.emit("dl_cancelled", map[string]interface{}{"index": index})
					return false, false
				}
			}
		}
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{
		"index": index, "folder": courseDir,
	})
	return true, false
}

// kiwifyDownloadBatch downloads multiple courses sequentially.
// Stops on cancellation or if a download fails.
func (a *App) kiwifyDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})

	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.kiwifyDownloadOne(idx) {
			break
		}
	}

	a.emit("batch_done", map[string]interface{}{})
}
