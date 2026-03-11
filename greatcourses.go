package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	gcBaseURL   = "https://www.thegreatcoursesplus.com"
	gcM2APIURL  = "https://m2api.thegreatcourses.com"
	gcMediaURL  = "https://link.theplatform.com/s/jESqeC/media/guid/2661884195"
	gcImagesURL = "https://secureimages.teach12.com/tgc/images/m2/wondrium/courses"
)

// GreatCoursesState holds authentication token and cached course list.
type GreatCoursesState struct {
	token   string
	courses []GreatCoursesCourse
}

// GreatCoursesCourse represents a single course from the user's watchlist.
type GreatCoursesCourse struct {
	ID           string
	Name         string
	ImageURL     string
	GuidebookURL string
	FolderName   string
	Lectures     []gcLecture
	detailLoaded bool
}

// gcLecture holds lecture metadata from the course details API.
type gcLecture struct {
	Number      int
	Name        string
	VideoID     string
	Description string
}

// greatcoursesHeaders returns common HTTP headers for Great Courses API requests.
func (a *App) greatcoursesHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + a.greatcourses.token,
		"Accept":        "application/json",
	}
}

// greatcoursesLogin authenticates with email/password and stores the bearer token.
func (a *App) greatcoursesLogin(email, password string) map[string]interface{} {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return map[string]interface{}{"success": false, "error": "Enter your email and password."}
	}

	url := gcBaseURL + "/rest/V1/integration/customer/token"
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, email, password)

	resp, err := httpRequest("POST", url, strings.NewReader(body), map[string]string{
		"Content-Type": "application/json",
	}, true)
	if err != nil {
		return map[string]interface{}{"success": false, "error": "Connection error: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("Login failed (HTTP %d)", resp.StatusCode)}
	}

	var token string
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return map[string]interface{}{"success": false, "error": "Invalid response from server."}
	}
	token = strings.Trim(token, "\" \n\r")
	if token == "" {
		return map[string]interface{}{"success": false, "error": "Empty token received."}
	}

	a.greatcourses.token = token
	return map[string]interface{}{"success": true}
}

// greatcoursesGetCourses fetches the user's watchlist (lightweight).
// Course details (lectures) are fetched lazily during download.
func (a *App) greatcoursesGetCourses() []map[string]interface{} {
	hdrs := a.greatcoursesHeaders()

	// Fetch watchlist
	data, status, err := httpGetJSON(gcBaseURL+"/rest/all/V2/watchlist/mine/items", hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var watchlist map[string]interface{}
	if json.Unmarshal(data, &watchlist) != nil {
		return nil
	}

	items := jsonArray(watchlist, "items")
	if len(items) == 0 {
		return nil
	}

	var courses []GreatCoursesCourse

	for _, item := range items {
		im, _ := item.(map[string]interface{})
		if im == nil {
			continue
		}

		courseID := jsonStr(im, "course_id")
		if courseID == "" {
			courseID = jsonStr(im, "product_sku")
		}
		if courseID == "" {
			continue
		}

		name := strings.TrimSpace(jsonStr(im, "course_name"))
		if name == "" {
			name = "Unknown Course"
		}

		imageURL := fmt.Sprintf("%s/%s/%s.jpg", gcImagesURL, courseID, courseID)
		folderName := sanitizeFilename(name)

		courses = append(courses, GreatCoursesCourse{
			ID:         courseID,
			Name:       name,
			ImageURL:   imageURL,
			FolderName: folderName,
		})
	}

	a.greatcourses.courses = courses

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

// greatcoursesLoadDetails fetches full course details (lectures, guidebook)
// from the m2api endpoint. Called lazily before download.
func (a *App) greatcoursesLoadDetails(index int) bool {
	course := &a.greatcourses.courses[index]
	if course.detailLoaded {
		return true
	}

	hdrs := a.greatcoursesHeaders()
	detailData, detailStatus, detailErr := httpGetJSON(
		gcM2APIURL+"/rest/all/V1/dlo_products/"+course.ID, hdrs,
	)
	if detailErr != nil || detailStatus != 200 {
		return false
	}

	var detail map[string]interface{}
	if json.Unmarshal(detailData, &detail) != nil {
		return false
	}

	course.GuidebookURL = jsonStr(detail, "course_guidebook_path")

	var lectures []gcLecture
	rawLectures := jsonArray(detail, "lectures")
	for _, rl := range rawLectures {
		lm, _ := rl.(map[string]interface{})
		if lm == nil {
			continue
		}
		num := 0
		if n, ok := lm["lecture_number"].(float64); ok {
			num = int(n)
		}
		lectures = append(lectures, gcLecture{
			Number:      num,
			Name:        jsonStr(lm, "lecture_name"),
			VideoID:     jsonStr(lm, "lecture_video_filename"),
			Description: jsonStr(lm, "lecture_description"),
		})
	}

	course.Lectures = lectures
	course.detailLoaded = true
	return true
}

// greatcoursesVideoURL returns the theplatform media URL for a given video ID.
func greatcoursesVideoURL(videoID string) string {
	return gcMediaURL + "/" + videoID + "?manifest=m3u"
}

// --- Download Orchestration ---

func (a *App) greatcoursesDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.greatcoursesDownloadOne(idx) {
			if a.cancel.Load() {
				break
			}
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}

func (a *App) greatcoursesDownloadOne(index int) bool {
	if index < 0 || index >= len(a.greatcourses.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course."})
		return false
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ok, retry := a.greatcoursesDownloadOneAttempt(index)
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

func (a *App) greatcoursesDownloadOneAttempt(index int) (ok bool, retryable bool) {
	defer func() {
		if r := recover(); r != nil {
			a.emit("dl_error", map[string]interface{}{
				"index": index, "message": fmt.Sprintf("Error: %v", r),
			})
			ok = false
			retryable = true
		}
	}()

	// Fetch full course details (lectures) if not yet loaded
	if !a.greatcoursesLoadDetails(index) {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": "Failed to fetch course details",
		})
		return false, true
	}

	course := a.greatcourses.courses[index]
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

	// Count real lectures (number > 0)
	totalLectures := 0
	for _, l := range course.Lectures {
		if l.Number > 0 {
			totalLectures++
		}
	}

	currentItem := 0
	totalItems := totalLectures + 2 // +2 for guidebook + cover

	// Download guidebook PDF
	if course.GuidebookURL != "" {
		currentItem++
		pdfPath := filepath.Join(courseDir, "guidebook.pdf")
		if _, err := os.Stat(pdfPath); os.IsNotExist(err) {
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentItem, "total": totalItems,
				"filename": "guidebook.pdf", "percent": 0,
			})
			downloadToFileSimple(course.GuidebookURL, pdfPath, nil)
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": currentItem, "total": totalItems,
			"filename": "guidebook.pdf", "percent": 100,
		})
	}

	// Download course cover image
	currentItem++
	coverPath := filepath.Join(courseDir, "cover.jpg")
	if _, err := os.Stat(coverPath); os.IsNotExist(err) {
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": currentItem, "total": totalItems,
			"filename": "cover.jpg", "percent": 0,
		})
		downloadToFileSimple(course.ImageURL, coverPath, nil)
	}
	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": currentItem, "total": totalItems,
		"filename": "cover.jpg", "percent": 100,
	})

	// Download lectures
	for _, lecture := range course.Lectures {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false, false
		}

		if lecture.Number == 0 {
			// Trailer
			videoID := lecture.VideoID
			if videoID == "" {
				videoID = course.ID
			}
			trailerPath := filepath.Join(courseDir, "00 - Trailer.mp4")
			if _, err := os.Stat(trailerPath); os.IsNotExist(err) {
				a.emit("dl_progress", map[string]interface{}{
					"index": index, "current": currentItem, "total": totalItems,
					"filename": "Trailer", "percent": 0,
				})
				a.greatcoursesDownloadVideo(videoID, trailerPath, index, currentItem, totalItems, "Trailer")
			}
			continue
		}

		currentItem++
		lectureName := lecture.Name
		if lectureName == "" {
			lectureName = fmt.Sprintf("Lecture %d", lecture.Number)
		}

		safeName := sanitizeFilename(fmt.Sprintf("%02d - %s", lecture.Number, lectureName))
		lectureDir := filepath.Join(courseDir, safeName)
		os.MkdirAll(lectureDir, 0755)

		// Download video
		if lecture.VideoID != "" {
			videoPath := filepath.Join(lectureDir, "video.mp4")
			if _, err := os.Stat(videoPath); os.IsNotExist(err) {
				a.greatcoursesDownloadVideo(lecture.VideoID, videoPath, index, currentItem, totalItems, lectureName)
			} else {
				a.emit("dl_progress", map[string]interface{}{
					"index": index, "current": currentItem, "total": totalItems,
					"filename": lectureName + " (already downloaded)", "percent": 100,
				})
			}
		}

		// Download lecture cover image
		lectureImgURL := fmt.Sprintf("%s/%s/%s-L%02d.jpg", gcImagesURL, course.ID, course.ID, lecture.Number)
		lectureImgPath := filepath.Join(lectureDir, "cover.jpg")
		if _, err := os.Stat(lectureImgPath); os.IsNotExist(err) {
			downloadToFileSimple(lectureImgURL, lectureImgPath, nil)
		}

		// Save description
		if lecture.Description != "" {
			descPath := filepath.Join(lectureDir, "description.txt")
			if _, err := os.Stat(descPath); os.IsNotExist(err) {
				os.WriteFile(descPath, []byte(lectureName+"\n"+strings.Repeat("=", len(lectureName))+"\n\n"+lecture.Description+"\n"), 0644)
			}
		}
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseDir})
	return true, false
}

// greatcoursesDownloadVideo resolves theplatform URL to Akamai and downloads via yt-dlp.
func (a *App) greatcoursesDownloadVideo(videoID, outputPath string, index, current, total int, filename string) {
	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": filename, "percent": 0,
	})

	videoURL := greatcoursesVideoURL(videoID)

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

	// Use yt-dlp with --force-generic-extractor to handle theplatform redirect.
	// Limit to 720p mp4 for faster merge.
	args := []string{
		"--no-check-certificates",
		"--no-warnings",
		"--force-generic-extractor",
		"--progress", "--newline",
		"-S", "height:720",
		"--concurrent-fragments", "8",
		"--merge-output-format", "mp4",
		"-o", outputPath,
		videoURL,
	}

	err := a.runYtdlpArgs(args, progress)
	if err != nil {
		cleanYtdlpTempFiles(outputPath)
		if a.cancel.Load() || strings.Contains(err.Error(), "cancelled") {
			return
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": filename + " (FAILED)", "percent": 0,
		})
		return
	}

	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": filename, "percent": 100,
	})
}
