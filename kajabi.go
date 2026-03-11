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

// KajabiState holds auth state and cached data for Kajabi.
// Kajabi organizes courses under "sites" — users may have access to multiple sites.
type KajabiState struct {
	token     string
	email     string
	loginData map[string]interface{} // response from step 1 (contains login token)
	sites     []map[string]interface{}
	siteID    string // currently selected site
	courses   []KajabiCourse
}

// KajabiCourse represents a single course (product) on Kajabi.
type KajabiCourse struct {
	Raw        map[string]interface{} // original JSON from the API
	ID         string
	Name       string
	ImageURL   string
	FolderName string
}

// kajabiHeaders returns HTTP headers that mimic the Kajabi mobile app.
// Includes custom headers (Kjb-App-Id, KJB-DP, KJB-SITE-ID) required by the API.
func (a *App) kajabiHeaders() map[string]string {
	h := map[string]string{
		"User-Agent":           "KajabiMobileApp",
		"Kjb-App-Id":          "Kajabi",
		"KJB-DP":              "ANDROID",
		"Content-Type":        "application/json",
		"KJB-SITE-ID":         a.kajabi.siteID,
		"Kjb-Template-Variant": "KMA",
		"template_version":    "4.7.3",
	}
	if a.kajabi.token != "" {
		h["Authorization"] = a.kajabi.token
	}
	return h
}

// kajabiAPIGet performs a GET request to the Kajabi API with automatic retry
// on 403 (forbidden) and 429 (rate limit) responses, up to 3 attempts
// with exponential backoff.
func (a *App) kajabiAPIGet(url string) ([]byte, int, error) {
	hdrs := a.kajabiHeaders()
	var data []byte
	var status int
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		data, status, err = httpGetJSON(url, hdrs)
		if status == 403 || status == 429 {
			time.Sleep(time.Duration(3*(attempt+1)) * time.Second)
			continue
		}
		return data, status, err
	}
	return data, status, err
}

// kajabiFetchSites retrieves the list of sites the user has access to.
// The API response can be either a JSON array or an object with "data"/"sites" key.
func (a *App) kajabiFetchSites() []map[string]interface{} {
	data, status, err := a.kajabiAPIGet("https://app.kajabi.com/api/mobile/v2/sites")
	if err != nil || status != 200 {
		return nil
	}
	var parsed interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	// Handle both array and object response formats
	var sites []interface{}
	switch v := parsed.(type) {
	case []interface{}:
		sites = v
	case map[string]interface{}:
		if d := jsonArray(v, "data"); d != nil {
			sites = d
		} else if s := jsonArray(v, "sites"); s != nil {
			sites = s
		}
	}

	var result []map[string]interface{}
	for _, s := range sites {
		if sm, ok := s.(map[string]interface{}); ok {
			result = append(result, sm)
		}
	}
	return result
}

// kajabiLogin handles the 2-step OTP authentication flow:
//   - Step 1: Login("email", "") → sends login link/OTP to email
//   - Step 2: Login("email", "code") → verifies OTP and gets Bearer token
//
// On success, fetches sites and returns them as "subgroups" for the frontend.
func (a *App) kajabiLogin(email, password string) map[string]interface{} {
	if password == "" && email == "" {
		return map[string]interface{}{"success": false, "error": "No saved session"}
	}

	if password == "" {
		// Step 1: Request OTP / login link
		hdrs := a.kajabiHeaders()
		body, _ := json.Marshal(map[string]string{"email": email})
		resp, err := httpRequest("POST", "https://app.kajabi.com/api/mobile/v2/login_links",
			bytes.NewReader(body), hdrs, true)
		if err != nil {
			return map[string]interface{}{"success": false, "error": err.Error()}
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return map[string]interface{}{"success": false, "error": fmt.Sprintf("Error sending OTP (HTTP %d)", resp.StatusCode)}
		}

		var loginResp map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&loginResp)
		a.kajabi.loginData = loginResp
		a.kajabi.email = email
		return map[string]interface{}{"success": false, "otp_sent": true}
	}

	// Step 2: Verify OTP / confirmation code
	otpEmail := email
	if otpEmail == "" {
		otpEmail = a.kajabi.email
	}

	authBody := map[string]interface{}{
		"email":             otpEmail,
		"confirmation_code": password,
	}

	// Include login token from step 1 if available
	if a.kajabi.loginData != nil {
		loginToken := jsonStr(a.kajabi.loginData, "token")
		if loginToken == "" {
			loginToken = jsonStr(a.kajabi.loginData, "login_token")
		}
		if loginToken == "" {
			if d := jsonMap(a.kajabi.loginData, "data"); d != nil {
				loginToken = jsonStr(d, "token")
			}
		}
		if loginToken != "" {
			authBody["token"] = loginToken
		}
	}

	hdrs := a.kajabiHeaders()
	body, _ := json.Marshal(authBody)
	resp, err := httpRequest("POST", "https://app.kajabi.com/api/mobile/v2/authentication",
		bytes.NewReader(body), hdrs, true)
	if err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("OTP verification failed (HTTP %d)", resp.StatusCode)}
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	// Extract bearer token — Kajabi returns it under various keys
	bt := jsonStr(result, "bearerToken")
	if bt == "" {
		bt = jsonStr(result, "token")
	}
	if bt == "" {
		bt = jsonStr(result, "access_token")
	}
	if bt == "" {
		bt = jsonStr(result, "bearer_token")
	}
	if bt == "" {
		if d := jsonMap(result, "data"); d != nil {
			bt = jsonStr(d, "token")
			if bt == "" {
				bt = jsonStr(d, "bearerToken")
			}
		}
	}
	if bt == "" {
		return map[string]interface{}{"success": false, "error": "Token not found in response"}
	}

	if !strings.HasPrefix(bt, "Bearer ") {
		bt = "Bearer " + bt
	}
	a.kajabi.token = bt
	a.kajabi.email = otpEmail

	// Fetch sites
	sites := a.kajabiFetchSites()
	if len(sites) == 0 {
		return map[string]interface{}{"success": false, "error": "No sites found"}
	}

	a.kajabi.sites = sites
	var subgroups []map[string]interface{}
	for _, s := range sites {
		subgroups = append(subgroups, map[string]interface{}{
			"name": jsonStr(s, "title"),
			"id":   jsonStr(s, "id"),
		})
	}

	return map[string]interface{}{"success": true, "subgroups": subgroups}
}

// kajabiGetCourses fetches all courses (products) for the currently selected site.
// Handles pagination (50 per page) and auto-selects the first site if needed.
func (a *App) kajabiGetCourses() []map[string]interface{} {
	if a.kajabi.siteID == "" && len(a.kajabi.sites) > 0 {
		a.kajabi.siteID = jsonStr(a.kajabi.sites[0], "id")
	}

	var allCourses []interface{}
	page := 1
	for {
		url := fmt.Sprintf("https://mobile-api.kajabi.com/api/mobile/v3/sites/%s/courses?page=%d&per_page=50",
			a.kajabi.siteID, page)
		data, status, err := a.kajabiAPIGet(url)
		if err != nil || status != 200 {
			break
		}
		var parsed map[string]interface{}
		if json.Unmarshal(data, &parsed) != nil {
			break
		}
		courses := jsonArray(parsed, "data")
		if len(courses) == 0 {
			break
		}
		allCourses = append(allCourses, courses...)
		if parsed["next_page"] == nil {
			break
		}
		page++
	}

	var courses []KajabiCourse
	for _, c := range allCourses {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		name := jsonStr(cm, "title")
		if name == "" {
			name = "unnamed"
		}
		kc := KajabiCourse{
			Raw:        cm,
			ID:         jsonStr(cm, "id"),
			Name:       name,
			ImageURL:   jsonStr(cm, "thumbnail_url"),
			FolderName: sanitizeFilename(name),
		}
		courses = append(courses, kc)
	}

	a.kajabi.courses = courses

	// Build frontend-friendly response
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

// --- Post/Lesson Download ---

// kajabiDownloadPost downloads a single post (lesson) from a Kajabi course.
// Each post may contain a video, HTML description, and downloadable attachments.
func (a *App) kajabiDownloadPost(siteID, productID string, post map[string]interface{},
	folder string, postNum, index, donePosts, totalPosts int) {

	postID := jsonStr(post, "id")
	url := fmt.Sprintf("https://mobile-api.kajabi.com/api/mobile/v3/sites/%s/products/%s/posts/%s",
		siteID, productID, postID)
	data, status, err := a.kajabiAPIGet(url)
	if err != nil || status != 200 {
		return
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return
	}
	detail := jsonMap(parsed, "data")
	if detail == nil {
		detail = parsed
	}

	rawTitle := sanitizeFilename(jsonStr(detail, "title"))
	if rawTitle == "" {
		rawTitle = "post_" + postID
	}
	title := fmt.Sprintf("%02d - %s", postNum, rawTitle)

	progress := func(dl, tb int64) {
		pct := 50.0
		if tb > 0 {
			pct = float64(dl) / float64(tb) * 100.0
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": donePosts + 1, "total": totalPosts,
			"filename": title, "percent": pct,
		})
	}

	// --- Find video URL ---
	// Kajabi stores video URLs in multiple possible fields
	videoURL := jsonStr(detail, "video_url")
	if videoURL == "" {
		if vid := jsonMap(detail, "video"); vid != nil {
			videoURL = jsonStr(vid, "url")
		}
	}
	if videoURL == "" {
		if vid := jsonMap(detail, "wistia_video"); vid != nil {
			videoURL = jsonStr(vid, "url")
		}
	}
	if videoURL == "" {
		cvids := jsonArray(detail, "content_videos")
		if cvids == nil {
			cvids = jsonArray(detail, "videos")
		}
		if len(cvids) > 0 {
			if vm, ok := cvids[0].(map[string]interface{}); ok {
				videoURL = jsonStr(vm, "url")
				if videoURL == "" {
					videoURL = jsonStr(vm, "video_url")
				}
				if videoURL == "" {
					videoURL = jsonStr(vm, "hls_url")
				}
			}
		}
	}

	// Download video (try yt-dlp first, fall back to direct download)
	if videoURL != "" {
		vidPath := filepath.Join(folder, title+".mp4")
		if _, err := os.Stat(vidPath); os.IsNotExist(err) {
			os.MkdirAll(folder, 0755)
			err := a.runYtdlp(vidPath, videoURL, progress)
			if err != nil {
				cleanYtdlpTempFiles(vidPath)
				if !a.cancel.Load() {
					a.downloadToFile(videoURL, vidPath, nil, progress)
				}
			}
		}
	} else {
		// Try media assets (Wistia-style embedded videos)
		if media := jsonMap(detail, "media"); media != nil {
			if assets := jsonArray(media, "assets"); len(assets) > 0 {
				// Pick best quality: prefer HdMp4VideoFile > OriginalFile > first asset
				var best map[string]interface{}
				for _, asset := range assets {
					am, _ := asset.(map[string]interface{})
					if am == nil {
						continue
					}
					if jsonStr(am, "type") == "HdMp4VideoFile" {
						best = am
						break
					}
					if jsonStr(am, "type") == "OriginalFile" {
						best = am
					}
				}
				if best == nil {
					if am, ok := assets[0].(map[string]interface{}); ok {
						best = am
					}
				}
				if best != nil {
					assetURL := jsonStr(best, "url")
					if strings.HasPrefix(assetURL, "http://") {
						assetURL = "https://" + assetURL[7:]
					}
					ct := jsonStr(best, "content_type")
					ext := ".bin"
					if strings.Contains(ct, "video") {
						ext = ".mp4"
					}
					assetPath := filepath.Join(folder, title+ext)
					if _, err := os.Stat(assetPath); os.IsNotExist(err) {
						os.MkdirAll(folder, 0755)
						a.downloadToFile(assetURL, assetPath, nil, progress)
					}
				}
			}
		}
	}

	// Save HTML description
	desc := jsonStr(detail, "description")
	if desc != "" {
		descPath := filepath.Join(folder, title+".html")
		if _, err := os.Stat(descPath); os.IsNotExist(err) {
			os.MkdirAll(folder, 0755)
			os.WriteFile(descPath, []byte(desc), 0644)
		}
	}

	// Download file attachments
	downloads := jsonArray(detail, "downloads")
	for _, dl := range downloads {
		dm, _ := dl.(map[string]interface{})
		if dm == nil {
			continue
		}
		dlURL := jsonStr(dm, "url")
		if dlURL == "" || !strings.HasPrefix(dlURL, "http") {
			continue
		}
		dlName := sanitizeFilename(jsonStr(dm, "name"))
		if dlName == "" {
			dlName = "download"
		}
		dlPath := filepath.Join(folder, title+" - "+dlName)
		if _, err := os.Stat(dlPath); os.IsNotExist(err) {
			os.MkdirAll(folder, 0755)
			a.downloadToFile(dlURL, dlPath, nil, progress)
		}
	}
}

// --- Category/Module Download ---

// kajabiDownloadCategory recursively downloads all posts in a category
// (module) and its subcategories. Returns the updated donePosts count.
func (a *App) kajabiDownloadCategory(siteID, productID string, category map[string]interface{},
	parentFolder, catName string, index, donePosts, totalPosts int) int {

	catFolder := filepath.Join(parentFolder, catName)
	os.MkdirAll(catFolder, 0755)

	posts := jsonArray(category, "posts")
	subs := jsonArray(category, "subcategories")

	// If posts/subcategories aren't inline, fetch them from the API
	if len(posts) == 0 && len(subs) == 0 {
		catID := jsonStr(category, "id")
		url := fmt.Sprintf("https://mobile-api.kajabi.com/api/mobile/v3/sites/%s/products/%s/categories/%s/posts",
			siteID, productID, catID)
		data, status, err := a.kajabiAPIGet(url)
		if err == nil && status == 200 {
			var parsed map[string]interface{}
			if json.Unmarshal(data, &parsed) == nil {
				d := jsonMap(parsed, "data")
				if d != nil {
					posts = jsonArray(d, "posts")
					subs = jsonArray(d, "subcategories")
				}
			}
		}
	}

	// Download posts in this category
	for i, p := range posts {
		if a.cancel.Load() {
			return donePosts
		}
		pm, _ := p.(map[string]interface{})
		if pm == nil {
			continue
		}
		a.kajabiDownloadPost(siteID, productID, pm, catFolder, i+1, index, donePosts, totalPosts)
		donePosts++
	}

	// Recurse into subcategories
	for j, s := range subs {
		sm, _ := s.(map[string]interface{})
		if sm == nil {
			continue
		}
		subTitle := sanitizeFilename(jsonStr(sm, "title"))
		if subTitle == "" {
			subTitle = fmt.Sprintf("sub_%d", j)
		}
		subName := fmt.Sprintf("%02d - %s", j+1, subTitle)
		donePosts = a.kajabiDownloadCategory(siteID, productID, sm, catFolder, subName, index, donePosts, totalPosts)
	}

	return donePosts
}

// --- Course Download Orchestration ---

// kajabiDownloadOne downloads a single Kajabi course.
// Structure: categories (modules) > posts (lessons), with optional subcategories.
func (a *App) kajabiDownloadOne(index int) bool {
	if index < 0 || index >= len(a.kajabi.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course."})
		return false
	}

	course := a.kajabi.courses[index]
	courseFolder := filepath.Join(a.platformDir(), course.FolderName)
	os.MkdirAll(courseFolder, 0755)

	a.emit("dl_started", map[string]interface{}{"index": index, "folder": courseFolder})

	// Fetch course categories (modules)
	url := fmt.Sprintf("https://mobile-api.kajabi.com/api/mobile/v3/sites/%s/products/%s/categories",
		a.kajabi.siteID, course.ID)
	data, status, err := a.kajabiAPIGet(url)
	if err != nil || status != 200 {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Failed to fetch categories"})
		return false
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid categories response"})
		return false
	}

	d := jsonMap(parsed, "data")
	if d == nil {
		d = parsed
	}
	categories := jsonArray(d, "categories")
	if len(categories) == 0 {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "No modules found"})
		return false
	}

	// Count total posts across all categories for progress
	totalPosts := 0
	for _, c := range categories {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		if tp, ok := cm["total_posts"].(float64); ok {
			totalPosts += int(tp)
		}
	}

	// Download each category recursively
	donePosts := 0
	for ci, c := range categories {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false
		}
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		catTitle := sanitizeFilename(jsonStr(cm, "title"))
		if catTitle == "" {
			catTitle = "module"
		}
		catName := fmt.Sprintf("%02d - %s", ci+1, catTitle)

		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": donePosts, "total": totalPosts,
			"filename": fmt.Sprintf("Module %d/%d: %s", ci+1, len(categories), jsonStr(cm, "title")),
			"percent": 0,
		})

		donePosts = a.kajabiDownloadCategory(a.kajabi.siteID, course.ID, cm,
			courseFolder, catName, index, donePosts, totalPosts)
	}

	markCourseComplete(courseFolder)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseFolder})
	return true
}

// kajabiDownloadBatch downloads multiple Kajabi courses sequentially.
func (a *App) kajabiDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.kajabiDownloadOne(idx) {
			if a.cancel.Load() {
				break
			}
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}
