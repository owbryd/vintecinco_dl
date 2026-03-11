package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Gumroad API constants.
// Authentication uses OAuth2 password grant. File downloads go through
// the mobile API which provides streaming URLs and direct download links.
const (
	gumroadClientID     = "46410c2fb9aa741c1f03cdea099929c795d20de0282b352eac881dfa46b2b89c"
	gumroadClientSecret = "e2fa7dc5bc347d09820a3931d4ce10e1137a02577ce647b33c60670a72b1acd5"
	gumroadMobileToken  = "ps407sr3rno1561ro2o4n360q21248s4o24oq33770rpro59o11q9r5469ososoo"
	gumroadStreamBase   = "https://api.gumroad.com/mobile/url_redirects/stream"
)

// gumroadMediaExt lists file extensions that should be streamed via yt-dlp
// rather than downloaded directly. Includes video and audio formats.
var gumroadMediaExt = map[string]bool{
	"mp4": true, "mkv": true, "avi": true, "mov": true, "webm": true,
	"mp3": true, "wav": true, "flac": true, "aac": true, "ogg": true, "m4a": true, "wma": true,
}

// GumroadState holds the OAuth access token and cached product list.
type GumroadState struct {
	token   string
	courses []GumroadCourse
}

// GumroadCourse represents a single purchased product on Gumroad.
// Gumroad calls them "products" but we use "course" for consistency.
type GumroadCourse struct {
	Raw        map[string]interface{} // original JSON from the API
	ID         string
	Name       string
	ImageURL   string
	FolderName string
}

// gumroadLogin authenticates with Gumroad using OAuth2 password grant.
// On success, stores the access token for subsequent API calls.
func (a *App) gumroadLogin(email, password string) map[string]interface{} {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", email)
	form.Set("password", password)
	form.Set("scope", "mobile_api creator_api")
	form.Set("client_id", gumroadClientID)
	form.Set("client_secret", gumroadClientSecret)

	resp, err := httpRequest("POST", "https://gumroad.com/oauth/token",
		strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, true)
	if err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if token, ok := result["access_token"].(string); ok && resp.StatusCode == 200 {
		a.gumroad.token = token
		return map[string]interface{}{"success": true}
	}
	return map[string]interface{}{"success": false, "error": "Authentication failed"}
}

// gumroadAPIHeaders returns HTTP headers for Gumroad API requests.
// Uses "okhttp/4.8.1" User-Agent to match the mobile app.
func (a *App) gumroadAPIHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + a.gumroad.token,
		"User-Agent":    "okhttp/4.8.1",
	}
}

// gumroadGetCourses fetches all purchased products from the Gumroad mobile API.
// Filters out refunded, chargedback, and archived products.
func (a *App) gumroadGetCourses() []map[string]interface{} {
	hdrs := a.gumroadAPIHeaders()
	apiURL := "https://api.gumroad.com/mobile/purchases/index.json" +
		"?include_subscriptions=true&include_mobile_unfriendly_products=true" +
		"&mobile_token=" + gumroadMobileToken

	data, status, err := httpGetJSON(apiURL, hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	success, _ := parsed["success"].(bool)
	if !success {
		return nil
	}

	products := jsonArray(parsed, "products")
	var courses []GumroadCourse

	for _, p := range products {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		// Skip cancelled/refunded purchases
		if b, _ := pm["refunded"].(bool); b {
			continue
		}
		if b, _ := pm["partially_refunded"].(bool); b {
			continue
		}
		if b, _ := pm["chargedback"].(bool); b {
			continue
		}
		if b, _ := pm["is_archived"].(bool); b {
			continue
		}

		name := jsonStr(pm, "name")
		if name == "" {
			name = "unnamed"
		}

		gc := GumroadCourse{
			Raw:        pm,
			ID:         jsonStr(pm, "id"),
			Name:       name,
			ImageURL:   jsonStr(pm, "preview_url"),
			FolderName: sanitizeFilename(name),
		}
		courses = append(courses, gc)
	}

	a.gumroad.courses = courses

	// Build frontend-friendly response
	result := make([]map[string]interface{}, len(courses))
	for i, c := range courses {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), c.FolderName))
		fileCount := 0
		if fd := jsonArray(c.Raw, "file_data"); fd != nil {
			fileCount = len(fd)
		}
		result[i] = map[string]interface{}{
			"name":        c.Name,
			"creator":     jsonStr(c.Raw, "creator_name"),
			"preview_url": c.ImageURL,
			"file_count":  fileCount,
			"downloaded":  dlStatus != "none",
			"dl_status":   dlStatus,
		}
	}
	return result
}

// gumroadDownloadOne downloads all files from a single Gumroad product.
// Media files (video/audio) are streamed via yt-dlp using the playlist URL.
// Other files are downloaded directly via the download redirect API.
func (a *App) gumroadDownloadOne(index int) bool {
	if index < 0 || index >= len(a.gumroad.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid product."})
		return false
	}

	course := a.gumroad.courses[index]
	productDir := filepath.Join(a.platformDir(), course.FolderName)
	os.MkdirAll(productDir, 0755)

	a.emit("dl_started", map[string]interface{}{"index": index, "folder": productDir})

	redirectToken := jsonStr(course.Raw, "url_redirect_token")
	fileData := jsonArray(course.Raw, "file_data")
	totalFiles := len(fileData)
	hdrs := a.gumroadAPIHeaders()

	for i, fd := range fileData {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false
		}

		fm, ok := fd.(map[string]interface{})
		if !ok {
			continue
		}

		filename := jsonStr(fm, "name")
		if filename == "" {
			filename = fmt.Sprintf("file_%d", i)
		}
		fileType := strings.ToLower(jsonStr(fm, "filetype"))
		fileGroup := strings.ToLower(jsonStr(fm, "filegroup"))
		fileID := jsonStr(fm, "id")
		destPath := filepath.Join(productDir, sanitizeFilename(filename))

		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": i + 1, "total": totalFiles,
			"filename": filename, "percent": 0,
		})

		// Skip already downloaded files
		if _, err := os.Stat(destPath); err == nil {
			continue
		}

		isMedia := fileGroup == "video" || fileGroup == "audio" || gumroadMediaExt[fileType]

		progress := func(dl, tb int64) {
			pct := 50.0
			if tb > 0 {
				pct = float64(dl) / float64(tb) * 100.0
			}
			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": i + 1, "total": totalFiles,
				"filename": filename, "percent": pct,
			})
		}

		if isMedia {
			// Stream media files: get playlist URL, then download with yt-dlp
			streamURL := gumroadStreamBase + "/" + redirectToken + "/" + fileID +
				"?mobile_token=" + gumroadMobileToken
			streamData, _, err := httpGetJSON(streamURL, hdrs)
			if err == nil {
				var sd map[string]interface{}
				if json.Unmarshal(streamData, &sd) == nil {
					playlistURL := jsonStr(sd, "playlist_url")
					if playlistURL != "" {
						os.MkdirAll(filepath.Dir(destPath), 0755)
						if err := a.runYtdlp(destPath, playlistURL, progress,
							"--add-header", "Authorization: Bearer "+a.gumroad.token,
							"--add-header", "User-Agent: okhttp/4.8.1"); err != nil {
							cleanYtdlpTempFiles(destPath)
						}
					}
				}
			}
		} else {
			// Direct file download via redirect API
			downloadURL := jsonStr(fm, "download_url")
			if downloadURL == "" {
				downloadURL = "https://api.gumroad.com/mobile/url_redirects/download/" +
					redirectToken + "/" + fileID + "?mobile_token=" + gumroadMobileToken
			}
			a.downloadToFile(downloadURL, destPath, hdrs, progress)
		}

		if a.cancel.Load() {
			os.Remove(destPath + ".part")
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false
		}
	}

	markCourseComplete(productDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": productDir})
	return true
}

// gumroadDownloadBatch downloads multiple Gumroad products sequentially.
func (a *App) gumroadDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.gumroadDownloadOne(idx) {
			break
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}
