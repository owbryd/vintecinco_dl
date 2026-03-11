package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Teachable API constants.
// Authentication uses a 2-step OTP flow: request OTP via email, then verify.
// The mobile API is used for all data access (courses, syllabus, lectures).
const (
	teachableClientToken = "9e44e885ac601aae4ee7109baec9ee0a503bfbb4fd11cbcb7d1de9d5e84f395b37d1521b08add19c2604dbe3c1d6c986bbd62a2513884e04e5b40704e77944e4"
	teachableBaseURL     = "https://mobile-service.learning.teachable.com/api/v1"
)

// TeachableState holds auth state and cached data for Teachable.
// Teachable organizes courses under "schools" — users may belong to multiple schools.
type TeachableState struct {
	token    string
	email    string
	schools  []map[string]interface{} // list of schools the user belongs to
	schoolID string                   // currently selected school
	courses  []TeachableCourse
}

// TeachableCourse represents a single course on Teachable.
type TeachableCourse struct {
	Raw        map[string]interface{} // original JSON from the API
	ID         string
	Name       string
	ImageURL   string
	FolderName string
}

// teachableAPIHeaders returns HTTP headers that mimic the Teachable mobile app.
// Uses custom headers (CLIENT-TOKEN, X-APP-VERSION, etc.) required by the API.
func (a *App) teachableAPIHeaders() map[string]string {
	return map[string]string{
		"Accept":             "application/json",
		"CLIENT-TOKEN":       teachableClientToken,
		"X-APP-VERSION":      "2.3.0",
		"X-APP-BUILD-NUMBER": "14",
		"X-DEVICE-OS":        "Android 35",
		"X-DEVICE-MODEL":     "motorola moto g75 5G",
		"X-TIMEZONE":         "America/Sao_Paulo",
		"Authorization":      "Bearer " + a.teachable.token,
		"Accept-Charset":     "UTF-8",
		"User-Agent":         "ktor-client",
		"Content-Type":       "application/json",
	}
}

// teachableLogin handles the 2-step OTP authentication flow:
//   - Step 1: Login("email", "") → sends OTP code to email, returns {otp_sent: true}
//   - Step 2: Login("email", "123456") → verifies OTP, returns {success: true, subgroups: [...]}
//
// On successful verification, fetches the user's schools and returns them
// as "subgroups" so the frontend can show a school selector.
func (a *App) teachableLogin(email, password string) map[string]interface{} {
	if password == "" {
		// Step 1: Request OTP
		body, _ := json.Marshal(map[string]string{"email": email})
		resp, err := httpRequest("POST", "https://sso.teachable.com/api/v2/auth/otp/request",
			bytes.NewReader(body),
			map[string]string{"Content-Type": "application/json", "User-Agent": "ktor-client"}, true)
		if err != nil {
			return map[string]interface{}{"success": false, "error": err.Error()}
		}
		resp.Body.Close()

		if resp.StatusCode == 200 {
			a.teachable.email = email
			return map[string]interface{}{"success": false, "otp_sent": true}
		}
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to send OTP (HTTP %d)", resp.StatusCode)}
	}

	// Step 2: Verify OTP code
	otpEmail := email
	if otpEmail == "" {
		otpEmail = a.teachable.email
	}

	body, _ := json.Marshal(map[string]string{"email": otpEmail, "otp_code": password})
	resp, err := httpRequest("POST", "https://sso.teachable.com/api/v2/auth/otp/verify",
		bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json", "User-Agent": "ktor-client"}, true)
	if err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	token, _ := result["token"].(string)
	if token == "" || resp.StatusCode != 200 {
		return map[string]interface{}{"success": false, "error": "OTP verification failed"}
	}

	a.teachable.token = token
	a.teachable.email = otpEmail

	// Fetch user's schools (a school is a "site" in Teachable)
	hdrs := a.teachableAPIHeaders()
	data, status, err := httpGetJSON(teachableBaseURL+"/schools", hdrs)
	if err != nil || status != 200 {
		return map[string]interface{}{"success": false, "error": "Failed to fetch schools"}
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return map[string]interface{}{"success": false, "error": "Invalid schools response"}
	}

	schools := jsonArray(parsed, "schools")
	if len(schools) == 0 {
		return map[string]interface{}{"success": false, "error": "No schools found"}
	}

	// Store schools and return as selectable subgroups
	a.teachable.schools = nil
	var subgroups []map[string]interface{}
	for _, s := range schools {
		sm, _ := s.(map[string]interface{})
		if sm != nil {
			a.teachable.schools = append(a.teachable.schools, sm)
			subgroups = append(subgroups, map[string]interface{}{
				"name":  jsonStr(sm, "name"),
				"id":    jsonStr(sm, "id"),
				"image": jsonStr(sm, "image"),
			})
		}
	}

	return map[string]interface{}{"success": true, "subgroups": subgroups}
}

// teachableGetCourses fetches all courses for the currently selected school.
// Auto-selects the first school if none is selected yet.
func (a *App) teachableGetCourses() []map[string]interface{} {
	if a.teachable.schoolID == "" && len(a.teachable.schools) > 0 {
		a.teachable.schoolID = jsonStr(a.teachable.schools[0], "id")
	}

	hdrs := a.teachableAPIHeaders()
	data, status, err := httpGetJSON(teachableBaseURL+"/schools/"+a.teachable.schoolID+"/courses", hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	coursesArr := jsonArray(parsed, "courses")
	var courses []TeachableCourse

	for _, c := range coursesArr {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		name := jsonStr(cm, "name")
		if name == "" {
			name = "unnamed"
		}

		tc := TeachableCourse{
			Raw:        cm,
			ID:         jsonStr(cm, "id"),
			Name:       name,
			ImageURL:   jsonStr(cm, "image"),
			FolderName: sanitizeFilename(name),
		}
		courses = append(courses, tc)
	}

	a.teachable.courses = courses

	// Build frontend-friendly response with author info and progress
	result := make([]map[string]interface{}, len(courses))
	for i, c := range courses {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), c.FolderName))
		authorName, authorImage := "", ""
		if bio := jsonMap(c.Raw, "author_bio"); bio != nil {
			authorName = jsonStr(bio, "name")
			authorImage = jsonStr(bio, "profile_image_url")
		}
		progress := 0
		if pg := jsonMap(c.Raw, "progress"); pg != nil {
			if pct, ok := pg["progress_percentage"].(float64); ok {
				progress = int(pct)
			}
		}
		result[i] = map[string]interface{}{
			"name":         c.Name,
			"preview_url":  c.ImageURL,
			"downloaded":   dlStatus != "none",
			"dl_status":    dlStatus,
			"author_name":  authorName,
			"author_image": authorImage,
			"progress":     progress,
		}
	}
	return result
}

// teachableDownloadOne downloads a single Teachable course.
// Structure: syllabus → sections → lectures → attachments (videos, PDFs, HTML).
func (a *App) teachableDownloadOne(index int) bool {
	if index < 0 || index >= len(a.teachable.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course."})
		return false
	}

	course := a.teachable.courses[index]
	courseDir := filepath.Join(a.platformDir(), course.FolderName)
	os.MkdirAll(courseDir, 0755)

	a.emit("dl_started", map[string]interface{}{"index": index, "folder": courseDir})

	hdrs := a.teachableAPIHeaders()

	// Fetch syllabus (sections + lectures structure)
	data, status, err := httpGetJSON(teachableBaseURL+"/schools/"+a.teachable.schoolID+"/courses/"+course.ID+"/syllabus", hdrs)
	if err != nil || status != 200 {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": fmt.Sprintf("Error fetching syllabus (HTTP %d)", status),
		})
		return false
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid syllabus response"})
		return false
	}

	// Count total lectures for progress
	sections := jsonArray(parsed, "sections")
	totalLectures := 0
	for _, s := range sections {
		sm, _ := s.(map[string]interface{})
		totalLectures += len(jsonArray(sm, "lectures"))
	}

	currentLecture := 0

	// Iterate: sections > lectures > attachments
	for si, s := range sections {
		sm, _ := s.(map[string]interface{})
		sectionName := sanitizeFilename(jsonStr(sm, "name"))
		if sectionName == "" {
			sectionName = "Section"
		}
		sectionDir := filepath.Join(courseDir, fmt.Sprintf("%d. %s", si+1, sectionName))
		os.MkdirAll(sectionDir, 0755)

		lectures := jsonArray(sm, "lectures")
		for li, l := range lectures {
			currentLecture++

			if a.cancel.Load() {
				a.emit("dl_cancelled", map[string]interface{}{"index": index})
				return false
			}

			lm, _ := l.(map[string]interface{})
			lectureID := jsonStr(lm, "id")
			lectureName := sanitizeFilename(jsonStr(lm, "name"))
			if lectureName == "" {
				lectureName = "Lecture"
			}
			lectureDir := filepath.Join(sectionDir, fmt.Sprintf("%d. %s", li+1, lectureName))

			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentLecture, "total": totalLectures,
				"filename": lectureName, "percent": 0,
			})

			// Skip already downloaded lectures
			if info, err := os.Stat(lectureDir); err == nil && info.IsDir() {
				entries, _ := os.ReadDir(lectureDir)
				if len(entries) > 0 {
					continue
				}
			}

			os.MkdirAll(lectureDir, 0755)

			// Fetch lecture details (attachments list)
			ldata, lstatus, lerr := httpGetJSON(teachableBaseURL+"/schools/"+a.teachable.schoolID+
				"/courses/"+course.ID+"/lectures/"+lectureID, hdrs)
			if lerr != nil || lstatus != 200 {
				continue
			}

			var lparsed map[string]interface{}
			if json.Unmarshal(ldata, &lparsed) != nil {
				continue
			}

			lectureData := jsonMap(lparsed, "lecture")
			if lectureData == nil {
				lectureData = lparsed
			}

			attachments := jsonArray(lectureData, "attachments")
			if len(attachments) == 0 {
				os.Remove(lectureDir) // clean up empty dir
				continue
			}

			// Download each attachment based on its "kind"
			for ai, att := range attachments {
				if a.cancel.Load() {
					a.emit("dl_cancelled", map[string]interface{}{"index": index})
					return false
				}

				am, _ := att.(map[string]interface{})
				if am == nil {
					continue
				}
				attName := sanitizeFilename(jsonStr(am, "name"))
				if attName == "" {
					attName = fmt.Sprintf("file_%d", ai+1)
				}
				kind := jsonStr(am, "kind")
				attData := jsonMap(am, "data")
				if attData == nil {
					attData = map[string]interface{}{}
				}

				progress := func(dl, tb int64) {
					pct := 0.0
					if tb > 0 {
						pct = float64(dl) / float64(tb) * 100.0
					}
					a.emit("dl_progress", map[string]interface{}{
						"index": index, "current": currentLecture, "total": totalLectures,
						"filename": attName, "percent": pct,
					})
				}

				switch kind {
				case "video":
					// Video attachments: download via M3U8 with yt-dlp
					m3u8URL := jsonStr(attData, "url")
					referer := jsonStr(attData, "referer")
					mediaName := sanitizeFilename(jsonStr(attData, "media_name"))
					if mediaName == "" {
						mediaName = attName
					}
					outPath := filepath.Join(lectureDir, mediaName+".mp4")
					if _, err := os.Stat(outPath); err == nil || m3u8URL == "" {
						continue
					}

					var extraArgs []string
					extraArgs = append(extraArgs,
						"--fragment-retries", "10", "--buffer-size", "64K",
						"--http-chunk-size", "10M", "--throttled-rate", "100K")
					if referer != "" {
						refererURL := "https://" + referer + "/"
						origin := "https://" + referer
						extraArgs = append(extraArgs,
							"--referer", refererURL,
							"--add-header", "Origin: "+origin)
					}
					extraArgs = append(extraArgs, "--legacy-server-connect")
					if err := a.runYtdlp(outPath, m3u8URL, progress, extraArgs...); err != nil {
						cleanYtdlpTempFiles(outPath)
					}

				case "html":
					// HTML text attachments
					htmlText := jsonStr(attData, "text")
					if htmlText != "" {
						htmlPath := filepath.Join(lectureDir, fmt.Sprintf("description_%d.html", ai+1))
						if _, err := os.Stat(htmlPath); os.IsNotExist(err) {
							os.WriteFile(htmlPath, []byte(htmlText), 0644)
						}
					}

				case "pdf_embed":
					// Embedded PDF files
					pdfURL := jsonStr(attData, "url")
					if pdfURL != "" {
						fname := attName
						if !strings.HasSuffix(strings.ToLower(fname), ".pdf") {
							fname += ".pdf"
						}
						pdfPath := filepath.Join(lectureDir, fname)
						if _, err := os.Stat(pdfPath); os.IsNotExist(err) {
							a.downloadToFile(pdfURL, pdfPath, nil, progress)
						}
					}

				default:
					// Generic file attachments (zip, doc, etc.)
					fileURL := jsonStr(attData, "url")
					if fileURL == "" || fileURL[0] == '{' {
						continue
					}
					ext := jsonStr(attData, "file_extension")
					fname := attName
					if ext != "" && !strings.Contains(fname, "."+ext) {
						fname += "." + ext
					}
					filePath := filepath.Join(lectureDir, fname)
					if _, err := os.Stat(filePath); os.IsNotExist(err) {
						a.downloadToFile(fileURL, filePath, nil, progress)
					}
				}
			}
		}
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseDir})
	return true
}

// teachableDownloadBatch downloads multiple Teachable courses sequentially,
// with up to 3 retries per course.
func (a *App) teachableDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})

		ok := false
		for attempt := 1; attempt <= 3; attempt++ {
			if a.teachableDownloadOne(idx) {
				ok = true
				break
			}
			if a.cancel.Load() {
				break
			}
			if attempt < 3 {
				time.Sleep(time.Duration(3*attempt) * time.Second)
			}
		}
		if !ok && a.cancel.Load() {
			break
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}
