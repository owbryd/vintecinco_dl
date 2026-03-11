package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Hotmart API constants.
// Unlike other platforms, Hotmart does not support headless login — the user
// must provide their hmVlcIntegration token (obtained from browser cookies).
// Course content is fetched via multiple API gateways (purchases, navigation,
// lessons, attachments).
const (
	hotmartUA            = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	hotmartPurchaseAPI   = "https://api-hub.cb.hotmart.com/club-drive-api/rest/v2/purchase/"
	hotmartCheckTokenAPI = "https://api-sec-vlc.hotmart.com/security/oauth/check_token"
	hotmartNavigationAPI = "https://api-club-course-consumption-gateway.hotmart.com/v1/navigation"
	hotmartLessonAPI     = "https://api-club-course-consumption-gateway.hotmart.com/v1/lesson"
	hotmartAttachmentAPI = "https://api-club-hot-club-api.cb.hotmart.com/rest/v3/attachment"
)

// HotmartState holds the auth token, cookies, subdomain list, and cached courses.
type HotmartState struct {
	token      string
	cookies    string        // optional cookie header for extra authentication
	subdomains []interface{} // product subdomains from check_token response
	courses    []HotmartCourse
}

// HotmartCourse represents a single purchased course on Hotmart.
type HotmartCourse struct {
	Raw        map[string]interface{} // original product JSON
	ID         string
	Name       string
	Slug       string // club subdomain used in API headers
	Seller     string
	Referer    string // required for Vimeo/embedded video authentication
	ImageURL   string
	FolderName string
}

// hotmartAPIHeaders returns common HTTP headers for Hotmart API requests.
func (a *App) hotmartAPIHeaders() map[string]string {
	h := map[string]string{
		"Authorization":    "Bearer " + a.hotmart.token,
		"User-Agent":       hotmartUA,
		"Accept":           "application/json, text/plain, */*",
		"Origin":           "https://consumer.hotmart.com",
		"Referer":          "https://consumer.hotmart.com/",
		"Pragma":           "no-cache",
		"Cache-Control":    "no-cache",
		"Accept-Language":  "pt-BR,pt;q=0.9",
		"Sec-Fetch-Site":   "same-site",
		"Sec-Fetch-Mode":   "cors",
		"Sec-Fetch-Dest":   "empty",
	}
	if a.hotmart.cookies != "" {
		h["Cookie"] = a.hotmart.cookies
	}
	return h
}

// hotmartCourseHeaders returns headers for course-specific API calls.
// Includes the "slug" and "x-product-id" headers required by the
// course consumption gateway.
func (a *App) hotmartCourseHeaders(slug, productID string) map[string]string {
	h := a.hotmartAPIHeaders()
	h["Origin"] = "https://hotmart.com"
	h["Referer"] = "https://hotmart.com"
	h["slug"] = slug
	h["x-product-id"] = productID
	return h
}

// hotmartAttachmentHeaders returns headers for attachment download API calls.
func (a *App) hotmartAttachmentHeaders() map[string]string {
	h := a.hotmartAPIHeaders()
	h["Accept-Language"] = "pt-BR,pt;q=0.8,en-US;q=0.5,en;q=0.3"
	return h
}

// hotmartLogin validates a Hotmart hmVlcIntegration token.
// Hotmart uses WebView SSO which cannot be automated headlessly.
// The "email" field is repurposed for the token, and "password" for optional cookies.
func (a *App) hotmartLogin(email, password string) map[string]interface{} {
	token := strings.TrimSpace(email)
	if token == "" {
		return map[string]interface{}{
			"success": false,
			"error":   "Please provide your hmVlcIntegration token. Login at https://consumer.hotmart.com in your browser, then copy the hmVlcIntegration cookie value from DevTools.",
		}
	}

	a.hotmart.token = token
	a.hotmart.cookies = strings.TrimSpace(password)

	// Validate token via check_token endpoint
	hdrs := a.hotmartAPIHeaders()
	checkURL := hotmartCheckTokenAPI + "?token=" + url.QueryEscape(token)
	data, status, err := httpGetJSON(checkURL, hdrs)
	if err != nil || (status != 200 && status != 0) {
		// Token may still work — try purchases API as fallback validation
		pdata, pstatus, perr := httpGetJSON(hotmartPurchaseAPI+"?archived=UNARCHIVED", hdrs)
		if perr != nil || pstatus != 200 {
			a.hotmart.token = ""
			return map[string]interface{}{"success": false, "error": fmt.Sprintf("Invalid token (HTTP %d)", pstatus)}
		}
		var pp map[string]interface{}
		if json.Unmarshal(pdata, &pp) == nil && jsonArray(pp, "data") != nil {
			return map[string]interface{}{"success": true}
		}
		a.hotmart.token = ""
		return map[string]interface{}{"success": false, "error": "Token validation failed"}
	}

	// Extract product subdomains from check_token response
	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) == nil {
		a.hotmart.subdomains = jsonArray(parsed, "resources")
	}

	return map[string]interface{}{"success": true}
}

// hotmartGetCourses fetches all purchased courses from the Hotmart purchase API.
// For each product, it resolves the club slug, seller name, and referer URL
// needed for downloading content.
func (a *App) hotmartGetCourses() []map[string]interface{} {
	hdrs := a.hotmartAPIHeaders()

	// Ensure subdomains are loaded (maps product IDs to club slugs)
	if len(a.hotmart.subdomains) == 0 {
		checkURL := hotmartCheckTokenAPI + "?token=" + url.QueryEscape(a.hotmart.token)
		data, _, _ := httpGetJSON(checkURL, hdrs)
		if data != nil {
			var parsed map[string]interface{}
			if json.Unmarshal(data, &parsed) == nil {
				a.hotmart.subdomains = jsonArray(parsed, "resources")
			}
		}
	}

	// Fetch purchase list
	data, status, err := httpGetJSON(hotmartPurchaseAPI+"?archived=UNARCHIVED", hdrs)
	if err != nil || status != 200 {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	purchases := jsonArray(parsed, "data")
	var courses []HotmartCourse

	for _, purchase := range purchases {
		pm, _ := purchase.(map[string]interface{})
		if pm == nil {
			continue
		}
		prod := jsonMap(pm, "product")
		if prod == nil {
			continue
		}

		pid := jsonStr(prod, "id")
		pname := strings.Join(strings.Fields(jsonStr(prod, "name")), " ")
		if pname == "" {
			pname = "Unnamed Course"
		}

		seller := ""
		if sellerObj := jsonMap(prod, "seller"); sellerObj != nil {
			seller = strings.Join(strings.Fields(jsonStr(sellerObj, "name")), " ")
		}

		picture := jsonStr(prod, "picture")

		// Get club slug from product or hotmartClub field
		var slug string
		if hc := jsonMap(prod, "hotmartClub"); hc != nil {
			slug = jsonStr(hc, "slug")
		}

		// Match product to subdomain from check_token response
		subdomain := ""
		for _, sub := range a.hotmart.subdomains {
			sm, _ := sub.(map[string]interface{})
			if sm == nil {
				continue
			}
			if res := jsonMap(sm, "resource"); res != nil {
				if jsonStr(res, "productId") == pid {
					subdomain = jsonStr(res, "subdomain")
					break
				}
			}
		}

		if subdomain == "" && slug == "" {
			continue
		}
		if slug == "" {
			slug = subdomain
		}

		// Get referer URL (needed for embedded video authentication)
		referer := ""
		pdetail, pstatus, _ := httpGetJSON(
			"https://api-hub.cb.hotmart.com/club-drive-api/rest/v2/purchase/products/"+pid, hdrs)
		if pstatus == 200 && pdetail != nil {
			var pd map[string]interface{}
			if json.Unmarshal(pdetail, &pd) == nil {
				if product := jsonMap(pd, "product"); product != nil {
					if mem := jsonMap(product, "membership"); mem != nil {
						referer = jsonStr(mem, "registerAddress")
					}
				}
			}
		}
		if referer == "" {
			referer = "https://" + slug + ".club.hotmart.com/"
		}

		folderName := sanitizeFilename(pname)
		if seller != "" {
			folderName += " - " + sanitizeFilename(seller)
		}

		hc := HotmartCourse{
			Raw:        prod,
			ID:         pid,
			Name:       pname,
			Slug:       slug,
			Seller:     seller,
			Referer:    referer,
			ImageURL:   picture,
			FolderName: folderName,
		}
		courses = append(courses, hc)
	}

	a.hotmart.courses = courses

	// Build frontend-friendly response
	result := make([]map[string]interface{}, len(courses))
	for i, c := range courses {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), c.FolderName))
		result[i] = map[string]interface{}{
			"name":        c.Name,
			"seller":      c.Seller,
			"preview_url": c.ImageURL,
			"downloaded":  dlStatus != "none",
			"dl_status":   dlStatus,
		}
	}
	return result
}

// --- Video/Iframe Extraction ---

// iframeInfo holds a parsed iframe source URL and its detected type
// (vimeo, panda, youtube, or other).
type iframeInfo struct {
	itype string
	url   string
}

// iframeRe matches <iframe> tags and captures the src attribute.
var iframeRe = regexp.MustCompile(`(?i)<iframe[^>]+src=["']([^"']+)["'][^>]*>`)

// extractIframes finds video iframes in HTML content from known providers
// (Vimeo, YouTube, PandaVideo, Wistia, etc.). Returns only iframes from
// whitelisted video hosting services.
func extractIframes(html string) []iframeInfo {
	allowList := []string{"vimeo", "youtu", "pandavideo", "liquid", "wistia", "videodelivery"}
	matches := iframeRe.FindAllStringSubmatch(html, -1)
	var results []iframeInfo

	for _, m := range matches {
		tag := m[0]
		src := m[1]

		inList := false
		for _, kw := range allowList {
			if strings.Contains(tag, kw) {
				inList = true
				break
			}
		}
		if !inList {
			continue
		}

		itype := "other"
		if strings.Contains(src, "vimeo.com") {
			itype = "vimeo"
		} else if strings.Contains(src, "pandavideo") {
			itype = "panda"
		} else if strings.Contains(src, "youtu") {
			itype = "youtube"
		}

		results = append(results, iframeInfo{itype: itype, url: src})
	}
	return results
}

// nextDataRe matches the __NEXT_DATA__ script tag used by Hotmart's Next.js
// player pages to embed media asset URLs in JSON.
var nextDataRe = regexp.MustCompile(`id="__NEXT_DATA__"[^>]*>(.*?)</script>`)

// extractNativeM3u8 fetches a Hotmart native player page and extracts
// the M3U8 playlist URL from the embedded __NEXT_DATA__ JSON.
func extractNativeM3u8(playerURL string) string {
	hdrs := map[string]string{
		"User-Agent": hotmartUA,
		"Referer":    "https://hotmart.com",
	}
	data, status, err := httpGetJSON(playerURL, hdrs)
	if err != nil || status != 200 {
		return ""
	}

	body := string(data)
	m := nextDataRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}

	var nd map[string]interface{}
	if json.Unmarshal([]byte(m[1]), &nd) != nil {
		return ""
	}

	// Navigate: props > pageProps > applicationData > mediaAssets[0] > url
	props := jsonMap(nd, "props")
	if props == nil {
		return ""
	}
	pageProps := jsonMap(props, "pageProps")
	if pageProps == nil {
		return ""
	}
	appData := jsonMap(pageProps, "applicationData")
	if appData == nil {
		return ""
	}
	assets := jsonArray(appData, "mediaAssets")
	if len(assets) == 0 {
		return ""
	}
	if am, ok := assets[0].(map[string]interface{}); ok {
		return jsonStr(am, "url")
	}
	return ""
}

// pandaToM3u8 converts a PandaVideo player URL to a direct M3U8 playlist URL.
// Transforms "player-vz-" to "b-vz-" and replaces "/embed/?v=" with a direct path.
func pandaToM3u8(url string) string {
	m := url
	if idx := strings.Index(m, "player-vz-"); idx != -1 {
		m = m[:idx] + "b-vz-" + m[idx+10:]
	}
	if idx := strings.Index(m, "/embed/?v="); idx != -1 {
		m = m[:idx] + "/" + m[idx+10:]
	}
	return m + "/playlist.m3u8"
}

// pandaReferer extracts the base domain from a PandaVideo URL
// for use as the Referer header when downloading the M3U8 stream.
func pandaReferer(url string) string {
	if idx := strings.Index(url, "com.br"); idx != -1 {
		return url[:idx+6]
	}
	return url
}

// --- Lesson Download ---

// hotmartDownloadAttachment downloads a single attachment file.
// Handles both regular downloads (directDownloadUrl) and DRM-protected
// files (lambdaUrl + token). Returns true on success.
func (a *App) hotmartDownloadAttachment(att map[string]interface{}, dir, slug, productID string) bool {
	attID := jsonStr(att, "fileMembershipId")
	attName := sanitizeFilename(jsonStr(att, "fileName"))
	if attName == "" {
		attName = "unknown"
	}
	if attID == "" {
		return false
	}

	savePath := filepath.Join(dir, attName)
	drmPath := filepath.Join(dir, "drm_"+attName)
	if _, err := os.Stat(savePath); err == nil {
		return true
	}
	if _, err := os.Stat(drmPath); err == nil {
		return true
	}

	os.MkdirAll(dir, 0755)
	hdrs := a.hotmartAttachmentHeaders()
	hdrs["slug"] = slug
	hdrs["x-product-id"] = productID

	data, status, err := httpGetJSON(hotmartAttachmentAPI+"/"+attID+"/download", hdrs)
	if err != nil || status != 200 {
		return false
	}

	var info map[string]interface{}
	if json.Unmarshal(data, &info) != nil {
		return false
	}

	body := string(data)
	if !strings.Contains(body, "drm-protection") {
		// Regular download
		dlURL := jsonStr(info, "directDownloadUrl")
		if dlURL != "" {
			return downloadToFileSimple(dlURL, savePath, map[string]string{"User-Agent": hotmartUA}) == nil
		}
	} else {
		// DRM-protected file — download via lambda URL with token
		lambdaURL := jsonStr(info, "lambdaUrl")
		drmToken := jsonStr(info, "token")
		if lambdaURL != "" {
			drmHdrs := map[string]string{
				"User-Agent":    hotmartUA,
				"token":         drmToken,
				"Authorization": "Bearer " + a.hotmart.token,
			}
			gdata, gstatus, _ := httpGetJSON(lambdaURL, drmHdrs)
			if gstatus != 500 && len(gdata) > 0 {
				return downloadToFileSimple(string(gdata), drmPath, map[string]string{"User-Agent": hotmartUA}) == nil
			}
		}
	}
	return false
}

// hotmartDownloadLesson downloads all content for a single Hotmart lesson:
//   - Attachments (materials/downloads with retry logic)
//   - Native Hotmart player videos (via M3U8 extraction)
//   - Embedded videos from iframes (Vimeo, PandaVideo, YouTube)
//   - HTML description
//   - Supplementary reading links
func (a *App) hotmartDownloadLesson(slug, productID, pageHash, refererURL string,
	lessonPath string, index, current, total int) error {

	hdrs := a.hotmartCourseHeaders(slug, productID)
	data, status, err := httpGetJSON(hotmartLessonAPI+"/"+pageHash, hdrs)
	if err != nil || status != 200 {
		return fmt.Errorf("HTTP %d", status)
	}

	var info map[string]interface{}
	if json.Unmarshal(data, &info) != nil {
		return fmt.Errorf("invalid JSON")
	}
	if msg := jsonStr(info, "message"); msg != "" {
		return fmt.Errorf("API: %s", msg)
	}

	os.MkdirAll(lessonPath, 0755)

	progress := func(dl, tb int64) {
		pct := 50.0
		if tb > 0 {
			pct = float64(dl) / float64(tb) * 100.0
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": filepath.Base(lessonPath), "percent": pct,
		})
	}

	// 1. Download attachments
	for _, att := range jsonArray(info, "attachments") {
		if a.cancel.Load() {
			return nil
		}
		am, _ := att.(map[string]interface{})
		if am == nil {
			continue
		}
		matDir := filepath.Join(lessonPath, "Materials")
		for attempt := 1; attempt <= 3; attempt++ {
			if a.hotmartDownloadAttachment(am, matDir, slug, productID) {
				break
			}
			if attempt < 3 {
				time.Sleep(time.Duration(2*attempt) * time.Second)
			}
		}
	}

	// 2. Download native Hotmart player videos (hasPlayerMedia flag)
	hasPlayer, _ := info["hasPlayerMedia"].(bool)
	if hasPlayer {
		mediaSrcs := jsonArray(info, "mediasSrc")
		for mi, media := range mediaSrcs {
			if a.cancel.Load() {
				return nil
			}
			mm, _ := media.(map[string]interface{})
			if mm == nil {
				continue
			}
			mediaType := jsonStr(mm, "mediaType")
			mediaURL := jsonStr(mm, "mediaSrcUrl")

			if strings.Contains(mediaType, "VIDEO") {
				outPath := filepath.Join(lessonPath, fmt.Sprintf("%d. Lesson.mp4", mi+1))
				if _, err := os.Stat(outPath); err == nil {
					continue
				}
				// Extract M3U8 from the native player page
				m3u8 := extractNativeM3u8(mediaURL)
				if m3u8 != "" {
					err := a.runYtdlpArgs([]string{
						"--no-check-certificates", "--progress", "--newline",
						"-f", "best[height<=720]/bestvideo[height<=720]+bestaudio/best",
						"--merge-output-format", "mp4",
						"-N", "64", "--retries", "10",
						"--fragment-retries", "10", "--buffer-size", "64K",
						"--http-chunk-size", "10M", "--throttled-rate", "100K",
						"--add-header", "User-Agent: " + hotmartUA,
						"--add-header", "Referer: https://cf-embed.play.hotmart.com/",
						"-o", outPath, m3u8,
					}, progress)
					if err != nil {
						cleanYtdlpTempFiles(outPath)
					}
				}
			} else if strings.Contains(mediaType, "AUDIO") {
				// Audio files: extract from Next.js player data
				mediaName := jsonStr(mm, "mediaName")
				audioHdrs := map[string]string{"User-Agent": hotmartUA, "Referer": "https://hotmart.com"}
				pdata, pstatus, _ := httpGetJSON(mediaURL, audioHdrs)
				if pstatus == 200 {
					pbody := string(pdata)
					nm := nextDataRe.FindStringSubmatch(pbody)
					if len(nm) >= 2 {
						var nd map[string]interface{}
						if json.Unmarshal([]byte(nm[1]), &nd) == nil {
							props := jsonMap(nd, "props")
							pp := jsonMap(props, "pageProps")
							appData := jsonMap(pp, "applicationData")
							for _, asset := range jsonArray(appData, "mediaAssets") {
								am, _ := asset.(map[string]interface{})
								ct := strings.ToLower(jsonStr(am, "content_type"))
								if !strings.Contains(ct, "audio") {
									continue
								}
								aurl := jsonStr(am, "url")
								outA := filepath.Join(lessonPath, fmt.Sprintf("%d. %s", mi+1, sanitizeFilename(mediaName)))
								if _, err := os.Stat(outA); os.IsNotExist(err) && aurl != "" {
									a.downloadToFile(aurl, outA, map[string]string{"User-Agent": hotmartUA}, progress)
								}
							}
						}
					}
				}
			}
		}
	}

	// 3. Download videos from iframes embedded in lesson content
	content := jsonStr(info, "content")
	if content != "" {
		iframes := extractIframes(content)
		for fi, iframe := range iframes {
			if a.cancel.Load() {
				return nil
			}
			num := fi + 1
			outPath := filepath.Join(lessonPath, fmt.Sprintf("%d. Lesson.mp4", num))
			if _, err := os.Stat(outPath); err == nil {
				continue
			}

			switch iframe.itype {
			case "vimeo":
				err := a.runYtdlpArgs([]string{
					"--no-check-certificates", "--progress", "--newline",
					"-f", "best[height<=720]/bestvideo[height<=720]+bestaudio/best",
					"--merge-output-format", "mp4",
					"-N", "64", "--retries", "10",
					"--fragment-retries", "10", "--buffer-size", "64K",
					"--http-chunk-size", "10M", "--throttled-rate", "100K",
					"--add-header", "User-Agent: " + hotmartUA,
					"--add-header", "Referer: " + refererURL,
					"-o", outPath, iframe.url,
				}, progress)
				if err != nil {
					cleanYtdlpTempFiles(outPath)
				}

			case "panda":
				m3u8 := pandaToM3u8(iframe.url)
				ref := pandaReferer(iframe.url)
				err := a.runYtdlpArgs([]string{
					"--no-check-certificates", "--progress", "--newline",
					"-f", "best[height<=720]/bestvideo[height<=720]+bestaudio/best",
					"--merge-output-format", "mp4",
					"-N", "64", "--retries", "10",
					"--fragment-retries", "10", "--buffer-size", "64K",
					"--http-chunk-size", "10M", "--throttled-rate", "100K",
					"--add-header", "User-Agent: " + hotmartUA,
					"--add-header", "Referer: " + ref,
					"-o", outPath, m3u8,
				}, progress)
				if err != nil {
					cleanYtdlpTempFiles(outPath)
				}

			case "youtube":
				outYT := filepath.Join(lessonPath, fmt.Sprintf("%d. Lesson", num))
				err := a.runYtdlpArgs([]string{
					"--no-check-certificates", "--progress", "--newline",
					"-f", "best[height<=720]/bestvideo[height<=720]+bestaudio/best",
					"--merge-output-format", "mp4",
					"-N", "64", "--retries", "10",
					"--fragment-retries", "10", "--buffer-size", "64K",
					"--http-chunk-size", "10M", "--throttled-rate", "100K",
					"-o", outYT + " [%(id)s].%(ext)s", iframe.url,
				}, progress)
				if err != nil {
					cleanYtdlpTempFiles(outYT)
				}
			}
		}
	}

	// 4. Save HTML description
	if content != "" {
		descPath := filepath.Join(lessonPath, "Description.html")
		if _, err := os.Stat(descPath); os.IsNotExist(err) {
			os.WriteFile(descPath, []byte(content), 0644)
		}
	}

	// 5. Save supplementary reading links as HTML
	readings := jsonArray(info, "complementaryReadings")
	if len(readings) > 0 {
		var html strings.Builder
		for _, r := range readings {
			rm, _ := r.(map[string]interface{})
			if rm == nil {
				continue
			}
			rurl := jsonStr(rm, "articleUrl")
			rname := jsonStr(rm, "articleName")
			html.WriteString(fmt.Sprintf(`<a href="%s">%s</a><br>`+"\n", rurl, rname))
		}
		if html.Len() > 0 {
			readPath := filepath.Join(lessonPath, "Supplementary reading.html")
			if _, err := os.Stat(readPath); os.IsNotExist(err) {
				os.WriteFile(readPath, []byte(html.String()), 0644)
			}
		}
	}

	return nil
}

// --- Course Download Orchestration ---

// hotmartDownloadOne downloads a single Hotmart course with up to 3 retries.
func (a *App) hotmartDownloadOne(index int) bool {
	if index < 0 || index >= len(a.hotmart.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course."})
		return false
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ok, retry := a.hotmartDownloadOneAttempt(index)
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

// hotmartDownloadOneAttempt performs a single download attempt for a Hotmart course.
// Fetches the course navigation tree (modules > pages), then iterates through
// each page downloading lesson content. Returns (ok, retryable).
func (a *App) hotmartDownloadOneAttempt(index int) (ok bool, retryable bool) {
	defer func() {
		if r := recover(); r != nil {
			a.emit("dl_error", map[string]interface{}{
				"index": index, "message": fmt.Sprintf("Error: %v", r),
			})
			ok = false
			retryable = true
		}
	}()

	course := a.hotmart.courses[index]
	courseDir := filepath.Join(a.platformDir(), course.FolderName)
	os.MkdirAll(courseDir, 0755)

	a.emit("dl_started", map[string]interface{}{"index": index, "folder": courseDir})

	// Clean leftover .part files
	filepath.Walk(courseDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".part" {
			os.Remove(path)
		}
		return nil
	})

	// Fetch course navigation tree (modules + pages)
	hdrs := a.hotmartCourseHeaders(course.Slug, course.ID)
	data, status, err := httpGetJSON(hotmartNavigationAPI, hdrs)
	if err != nil || status != 200 {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": fmt.Sprintf("Failed to fetch modules (HTTP %d)", status),
		})
		return false, true
	}

	var navData map[string]interface{}
	if json.Unmarshal(data, &navData) != nil {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid navigation response"})
		return false, true
	}

	modules := jsonArray(navData, "modules")
	if len(modules) == 0 {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Course has no modules"})
		return false, false
	}

	// Count total pages for progress display
	totalPages := 0
	for _, mod := range modules {
		mm, _ := mod.(map[string]interface{})
		totalPages += len(jsonArray(mm, "pages"))
	}

	currentPage := 0

	// Iterate modules > pages
	for mi, mod := range modules {
		mm, _ := mod.(map[string]interface{})
		modName := sanitizeFilename(jsonStr(mm, "name"))
		if modName == "" {
			modName = "Module"
		}
		modDir := filepath.Join(courseDir, fmt.Sprintf("%d. %s", mi+1, modName))

		pages := jsonArray(mm, "pages")
		for pi, page := range pages {
			if a.cancel.Load() {
				a.emit("dl_cancelled", map[string]interface{}{"index": index})
				return false, false
			}

			currentPage++
			pm, _ := page.(map[string]interface{})
			pageHash := jsonStr(pm, "hash")
			pageName := jsonStr(pm, "name")
			if pageName == "" {
				pageName = "Topic"
			}
			pageDir := filepath.Join(modDir, fmt.Sprintf("%d. %s", pi+1, sanitizeFilename(pageName)))

			// Skip already downloaded lessons
			if hotmartLessonDone(pageDir) {
				a.emit("dl_progress", map[string]interface{}{
					"index": index, "current": currentPage, "total": totalPages,
					"filename": pageName + " (already downloaded)", "percent": 100,
				})
				continue
			}

			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentPage, "total": totalPages,
				"filename": pageName, "percent": 0,
			})

			if pageHash != "" {
				for la := 1; la <= 3; la++ {
					err := a.hotmartDownloadLesson(course.Slug, course.ID, pageHash,
						course.Referer, pageDir, index, currentPage, totalPages)
					if err == nil {
						break
					}
					if la < 3 {
						time.Sleep(time.Duration(2*la) * time.Second)
					}
				}
			}
		}
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseDir})
	return true, false
}

// hotmartLessonDone checks if a Hotmart lesson has already been downloaded.
// Looks for video files, description, supplementary reading, or materials.
func hotmartLessonDone(dir string) bool {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false
	}

	// Clean leftover .part files
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".part" {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	// Re-read after cleanup so we don't use the stale slice to check for videos.
	entries, _ := os.ReadDir(dir)

	// Check for video files (any common video extension)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".mp4" || ext == ".mkv" || ext == ".webm" || ext == ".avi" {
			return true
		}
	}

	// Check for description or supplementary reading
	if _, err := os.Stat(filepath.Join(dir, "Description.html")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "Supplementary reading.html")); err == nil {
		return true
	}

	// Check for materials
	matDir := filepath.Join(dir, "Materials")
	if matEntries, err := os.ReadDir(matDir); err == nil {
		for _, e := range matEntries {
			if !e.IsDir() {
				return true
			}
		}
	}

	return false
}

// hotmartDownloadBatch downloads multiple Hotmart courses sequentially.
func (a *App) hotmartDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.hotmartDownloadOne(idx) {
			if a.cancel.Load() {
				break
			}
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}
