package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// ── Types ──────────────────────────────────────────────────────────────────

// ThinkificState holds session cookies, detected base URL, and cached course list.
type ThinkificState struct {
	cookies     string
	baseURL     string // e.g. "https://mysite.thinkific.com"
	courses     []ThinkificCourse
	cachedSlugs []string // slugs harvested from the live browser during login
}

// ThinkificCourse represents a single enrolled course.
type ThinkificCourse struct {
	Slug       string
	Name       string
	ImageURL   string
	FolderName string
}

// Pre-compiled regexes used inside thinkific functions.
var (
	thinkificBaseURLRe    = regexp.MustCompile(`(https?://[^/]+).*`)                       // strips path from URL
	thinkificPathRe       = regexp.MustCompile(`https?://[^/]+(.*)`)                       // captures path portion
	thinkificOGImageRe1   = regexp.MustCompile(`(?i)property=["']og:image["']\s+content=["']([^"']+)["']`)
	thinkificOGImageRe2   = regexp.MustCompile(`(?i)content=["']([^"']+)["']\s+property=["']og:image["']`)
	thinkificOGImageRe3   = regexp.MustCompile(`(?i)<meta[^>]+og:image[^>]+content=["']([^"']+)["']`)
	thinkificCoverOGRe    = regexp.MustCompile(`property=["']og:image["']\s+content=["']([^"']+)["']`)
	thinkificPlayerEmbRe  = regexp.MustCompile(`player\.thinkific\.com/embed/([a-f0-9-]+)`)
	thinkificMp4Re        = regexp.MustCompile(`https?://[^\s"']+\.mp4`)
)

// ── Login via Browser (chromedp) ────────────────────────────────────────────

// thinkificLogin opens Chrome via chromedp, navigates to the Thinkific login page,
// waits for the user to authenticate, then captures cookies automatically —
// exactly mirroring the Python Selenium browser_login() flow.
//
// The "email" field receives the site URL. You can pass:
//   - Just the domain: "mysite.thinkific.com"  → opens /users/sign_in
//   - Full login URL:  "mysite.thinkific.com/users/sign_in" (or any custom login path)
//   - With scheme:     "https://mysite.thinkific.com/my/login"
//
// The "password" field is ignored (login happens interactively in the browser).
func (a *App) thinkificLogin(siteURL, _ string) map[string]interface{} {
	rawURL := strings.TrimSpace(siteURL)
	if rawURL == "" {
		return map[string]interface{}{
			"success": false,
			"error":   "Enter your Thinkific site URL (e.g. mysite.thinkific.com).",
		}
	}

	// Extract the base URL (scheme + host only) for API calls later.
	baseURL := thinkificNormaliseURL(rawURL)

	// Determine the sign-in URL.
	// If the user supplied a path (anything after the host), use the full URL as-is.
	// Otherwise fall back to the default Thinkific login path.
	signInURL := thinkificExtractSignInURL(rawURL, baseURL)

	// ── Build Chrome allocator ─────────────────────────────────────────────
	// Use a dedicated, isolated profile for the downloader so we never
	// conflict with the user's existing Chrome session (which causes a second
	// window to open with all their restored tabs).
	headlessOff := chromedp.Flag("headless", false)

	// Try to find chrome.exe explicitly on Windows
	var chromeBin chromedp.ExecAllocatorOption
	for _, candidate := range []string{
		filepath.Join(os.Getenv("PROGRAMFILES"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			chromeBin = chromedp.ExecPath(candidate)
			break
		}
	}

	// Dedicated profile dir — persistent so cookies survive between runs,
	// but fully separate from the user's real Chrome profile.
	profileDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "vintecinco_dl", "chrome-profile")
	_ = os.MkdirAll(profileDir, 0755)

	opts := []chromedp.ExecAllocatorOption{
		headlessOff,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("exclude-switches", "enable-automation"),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("restore-last-session", false),
		chromedp.Flag("disable-session-crashed-bubble", true),
		chromedp.WindowSize(1100, 800),
	}
	if chromeBin != nil {
		opts = append(opts, chromeBin)
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(allocCtx)

	if err := chromedp.Run(ctx, chromedp.Navigate(signInURL)); err != nil {
		cancelCtx()
		cancelAlloc()
		return map[string]interface{}{
			"success": false,
			"error":   "Could not open Chrome. Make sure it is installed: " + err.Error(),
		}
	}

	// Close any extra tabs Chrome may have opened (e.g. an initial about:blank).
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tgts, err := chromedp.Targets(ctx)
		if err != nil {
			return nil // non-fatal
		}
		current := chromedp.FromContext(ctx)
		if current == nil || current.Target == nil {
			return nil
		}
		currentID := current.Target.TargetID
		for _, t := range tgts {
			if t.Type == "page" && t.TargetID != currentID {
				_ = target.CloseTarget(t.TargetID).Do(ctx)
			}
		}
		return nil
	}))


	defer func() {
		cancelCtx()
		cancelAlloc()
	}()

	// ── Poll until authenticated ───────────────────────────────────────────
	// Auth is confirmed when the session/remember cookies exist AND
	// the user has navigated away from the sign_in page.
	authCookieNames := map[string]bool{
		"remember_user_token": true,
		"_thinkific_session":  true,
	}

	const timeout = 5 * time.Minute
	const pollInterval = 1500 * time.Millisecond
	deadline := time.Now().Add(timeout)

	var capturedCookies []*network.Cookie
	loggedIn := false

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		var currentURL string
		var cookies []*network.Cookie

		err := chromedp.Run(ctx,
			chromedp.Location(&currentURL),
			chromedp.ActionFunc(func(ctx context.Context) error {
				var e error
				cookies, e = storage.GetCookies().Do(ctx)
				return e
			}),
		)
		if err != nil {
			// Window was closed — treat as cancellation.
			break
		}

		// Must have left the sign-in page.
		leftSignIn := !strings.Contains(currentURL, "sign_in") &&
			!strings.Contains(currentURL, "sign_up")

		// Must have at least one auth cookie.
		hasAuthCookie := false
		for _, c := range cookies {
			if authCookieNames[c.Name] {
				hasAuthCookie = true
				break
			}
		}

		if leftSignIn && hasAuthCookie {
			loggedIn = true
			capturedCookies = cookies
			break
		}
	}

	if !loggedIn {
		return map[string]interface{}{
			"success": false,
			"error":   "Login timeout or browser closed. Please try again.",
		}
	}

	// Let session cookies fully stabilise.
	time.Sleep(1 * time.Second)

	// ── Navigate to /enrollments to merge any new cookies ─────────────────
	_ = chromedp.Run(ctx,
		chromedp.Navigate(baseURL+"/enrollments"),
		chromedp.Sleep(3*time.Second),
		chromedp.ActionFunc(func(ctx context.Context) error {
			newCookies, e := storage.GetCookies().Do(ctx)
			if e != nil {
				return nil // non-fatal
			}
			existing := map[string]int{}
			for i, c := range capturedCookies {
				existing[c.Name] = i
			}
			for _, c := range newCookies {
				if idx, ok := existing[c.Name]; ok {
					capturedCookies[idx].Value = c.Value
				} else {
					capturedCookies = append(capturedCookies, c)
				}
			}
			return nil
		}),
	)

	// ── Build cookie header string ─────────────────────────────────────────
	var parts []string
	for _, c := range capturedCookies {
		if c.Value != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	cookieStr := strings.Join(parts, "; ")

	if cookieStr == "" {
		return map[string]interface{}{
			"success": false,
			"error":   "No cookies captured. Please try again.",
		}
	}

	a.thinkific.cookies = cookieStr
	a.thinkific.baseURL = baseURL

	// ── Harvest course slugs from the live browser (SPA rendered) ──────────
	// This is crucial for React/SPA Thinkific sites where plain HTTP requests
	// only get the empty pre-render HTML shell.
	var renderedHTML string
	_ = chromedp.Run(ctx,
		chromedp.Navigate(baseURL+"/enrollments"),
		chromedp.Sleep(4*time.Second), // wait for SPA hydration
		chromedp.OuterHTML("html", &renderedHTML, chromedp.ByQuery),
	)

	// Harvest slugs from the rendered HTML
	a.thinkific.cachedSlugs = thinkificHarvestSlugsFromHTML(renderedHTML)

	return map[string]interface{}{"success": true}
}

// thinkificNormaliseURL ensures a scheme is present and strips any path,
// returning only the base URL (scheme + host), e.g. "https://mysite.thinkific.com".
func thinkificNormaliseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	// Strip everything from the first path segment onward.
	raw = thinkificBaseURLRe.ReplaceAllString(raw, "$1")
	raw = strings.TrimRight(raw, "/")
	return raw
}

// thinkificExtractSignInURL decides which URL to open in Chrome for login.
// If the user supplied a custom path (e.g. mysite.com/my/login), that full URL
// is used directly. If only a bare domain was given, the default
// "<baseURL>/users/sign_in" is returned.
func thinkificExtractSignInURL(rawURL, baseURL string) string {
	// Ensure the raw URL has a scheme so we can parse it cleanly.
	withScheme := rawURL
	if !strings.HasPrefix(withScheme, "http://") && !strings.HasPrefix(withScheme, "https://") {
		withScheme = "https://" + withScheme
	}

	// Extract everything after the host (the path portion).
	pathPart := thinkificPathRe.ReplaceAllString(withScheme, "$1")
	pathPart = strings.TrimRight(pathPart, "/")

	// If the user gave us a real path, honour it.
	if pathPart != "" && pathPart != "/" {
		return baseURL + pathPart
	}

	// Default Thinkific login page.
	return baseURL + "/users/sign_in"
}

// ── HTTP Headers ───────────────────────────────────────────────────────────

func thinkificHeaders(cookies string) map[string]string {
	return map[string]string{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Accept":     "application/json",
		"Cookie":     cookies,
	}
}

// ── Course Listing ─────────────────────────────────────────────────────────

// thinkificHarvestSlugsFromHTML extracts all course slugs from rendered HTML.
// Uses a broad regex that catches all Thinkific URL patterns, then filters out
// purely-numeric tokens (which are lesson/content IDs, not course slugs).
func thinkificHarvestSlugsFromHTML(pageHTML string) []string {
	re := regexp.MustCompile(`(?i)/courses/(?:take/|enrolled/|enroll/)?([a-zA-Z][a-zA-Z0-9_-]{2,})\b`)

	skipSet := map[string]bool{
		"take": true, "new": true, "edit": true, "enroll": true,
		"preview": true, "sign_in": true, "sign_up": true, "enrolled": true,
		"progress": true, "admin": true, "users": true, "bundle": true,
		"checkout": true, "cart": true, "payment": true,
	}

	numericRe := regexp.MustCompile(`^\d+$`)
	seen := map[string]bool{}
	var slugs []string

	for _, m := range re.FindAllStringSubmatch(pageHTML, -1) {
		slug := m[1]
		lower := strings.ToLower(slug)
		// Skip purely numeric strings (lesson IDs, not slugs)
		if numericRe.MatchString(slug) {
			continue
		}
		if seen[lower] || skipSet[lower] {
			continue
		}
		seen[lower] = true
		slugs = append(slugs, slug)
	}
	return slugs
}

// thinkificSlugsToCourses resolves a list of slugs to full ThinkificCourse records
// by querying the course-player API for each slug.
// Slugs that return a non-200 or empty response are skipped (not real enrolled courses).
func (a *App) thinkificSlugsToCourses(slugs []string, hdrs map[string]string) []ThinkificCourse {
	seen := map[string]bool{}
	var courses []ThinkificCourse

	for _, slug := range slugs {
		if seen[slug] {
			continue
		}
		seen[slug] = true

		// Validate: only include slugs that actually resolve via the API
		detail, s, e := httpGetJSON(
			a.thinkific.baseURL+"/api/course_player/v2/courses/"+slug, hdrs,
		)
		if e != nil || s != 200 {
			continue // not a real enrolled course on this account
		}

		name := slug
		imageURL := ""

		var dm map[string]interface{}
		if json.Unmarshal(detail, &dm) == nil {
			if co, ok := dm["course"].(map[string]interface{}); ok {
				if n := jsonStr(co, "name"); n != "" {
					name = n
				}
				imageURL = thinkificExtractImage(co)
			} else {
				if n := jsonStr(dm, "name"); n != "" {
					name = n
				}
				imageURL = jsonStr(dm, "logo")
			}
		}

		courses = append(courses, ThinkificCourse{
			Slug:       slug,
			Name:       name,
			ImageURL:   imageURL,
			FolderName: sanitizeFilename(name),
		})
	}
	return courses
}

// thinkificExtractImage tries every known image field from a Thinkific API object.
// Different site versions / plans expose different field names.
func thinkificExtractImage(obj map[string]interface{}) string {
	for _, field := range []string{
		"logo",
		"course_card_image_url",
		"card_image_url",
		"banner_image_url",
		"image_url",
		"thumbnail_url",
		"cover_image_url",
		"image",
	} {
		if v := jsonStr(obj, field); v != "" {
			return v
		}
	}
	return ""
}

// thinkificScrapeOGImage fetches the HTML of a URL and returns the og:image content,
// or "" if none is found.
func thinkificScrapeOGImage(pageURL string, hdrs map[string]string) string {
	pageData, s, e := httpGetJSON(pageURL, hdrs)
	if e != nil || s != 200 {
		return ""
	}
	// Try both attribute orderings of <meta property="og:image" content="...">
	for _, re := range []*regexp.Regexp{
		thinkificOGImageRe1,
		thinkificOGImageRe2,
		thinkificOGImageRe3,
	} {
		if m := re.FindSubmatch(pageData); len(m) > 1 && string(m[1]) != "" {
			return string(m[1])
		}
	}
	return ""
}

func (a *App) thinkificGetCourses() []map[string]interface{} {
	hdrs := thinkificHeaders(a.thinkific.cookies)
	hdrs["Referer"] = a.thinkific.baseURL + "/"

	var courses []ThinkificCourse

	// Strategy 1: use slugs captured from the live browser during login
	// (most reliable — browser renders the SPA, HTTP requests only get shell HTML)
	if len(a.thinkific.cachedSlugs) > 0 {
		courses = a.thinkificSlugsToCourses(a.thinkific.cachedSlugs, hdrs)
	}

	// Strategy 2: enrollments JSON API
	if len(courses) == 0 {
		data, status, err := httpGetJSON(
			a.thinkific.baseURL+"/api/course_player/v2/enrollments", hdrs,
		)
		if err == nil && status == 200 {
			courses = a.thinkificParseEnrollments(data, hdrs)
		}
	}

	// Strategy 3: additional API variants
	if len(courses) == 0 {
		for _, endpoint := range []string{
			"/api/course_player/v2/courses",
			"/api/v0/enrollments",
			"/api/v0/courses",
		} {
			data, status, err := httpGetJSON(a.thinkific.baseURL+endpoint, hdrs)
			if err == nil && status == 200 {
				courses = a.thinkificParseEnrollments(data, hdrs)
				if len(courses) > 0 {
					break
				}
			}
		}
	}

	// Strategy 4: scrape /enrollments and /dashboard HTML via HTTP
	// (works for server-rendered Thinkific sites, not SPAs)
	if len(courses) == 0 {
		courses = a.thinkificScrapeEnrollments(hdrs)
	}

	a.thinkific.courses = courses

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


func (a *App) thinkificParseEnrollments(data []byte, hdrs map[string]string) []ThinkificCourse {
	var raw interface{}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}

	var items []interface{}
	switch v := raw.(type) {
	case []interface{}:
		items = v
	case map[string]interface{}:
		for _, key := range []string{"items", "enrollments", "courses"} {
			if arr, ok := v[key].([]interface{}); ok {
				items = arr
				break
			}
		}
	}

	var courses []ThinkificCourse
	seen := map[string]bool{}

	for _, item := range items {
		im, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		slug := jsonStr(im, "slug")
		if slug == "" {
			slug = jsonStr(im, "course_slug")
		}
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true

		name := jsonStr(im, "name")
		if name == "" {
			name = jsonStr(im, "course_name")
		}
		if name == "" {
			name = slug
		}
		imageURL := thinkificExtractImage(im)

		// Enrich from course detail API if name is still just the slug or imageURL is empty
		if name == slug || imageURL == "" {
			if detail, s, e := httpGetJSON(a.thinkific.baseURL+"/api/course_player/v2/courses/"+slug, hdrs); e == nil && s == 200 {
				var dm map[string]interface{}
				if json.Unmarshal(detail, &dm) == nil {
					if co, ok := dm["course"].(map[string]interface{}); ok {
						if n := jsonStr(co, "name"); n != "" {
							name = n
						}
						if imageURL == "" {
							imageURL = thinkificExtractImage(co)
						}
					} else {
						if n := jsonStr(dm, "name"); name == slug && n != "" {
							name = n
						}
						if imageURL == "" {
							imageURL = thinkificExtractImage(dm)
						}
					}
				}
			}
		}

		// Final fallback: scrape og:image from the course HTML page
		if imageURL == "" {
			imageURL = thinkificScrapeOGImage(a.thinkific.baseURL+"/courses/"+slug, hdrs)
		}

		courses = append(courses, ThinkificCourse{
			Slug:       slug,
			Name:       name,
			ImageURL:   imageURL,
			FolderName: sanitizeFilename(name),
		})
	}
	return courses
}

func (a *App) thinkificScrapeEnrollments(hdrs map[string]string) []ThinkificCourse {
	pageBody, status, err := httpGetJSON(a.thinkific.baseURL+"/enrollments", hdrs)
	if err != nil || status != 200 {
		return nil
	}
	pageHTML := string(pageBody)

	slugRe := regexp.MustCompile(`href=["']/?courses/(?:take|enrolled|enroll)?/?([^"'/?#]+)["']`)
	skipSet := map[string]bool{
		"take": true, "new": true, "edit": true, "enroll": true,
		"preview": true, "admin": true, "sign_in": true, "sign_up": true,
		"users": true, "enrolled": true, "progress": true,
	}

	var courses []ThinkificCourse
	seen := map[string]bool{}

	for _, m := range slugRe.FindAllStringSubmatch(pageHTML, -1) {
		slug := m[1]
		if slug == "" || seen[slug] || skipSet[strings.ToLower(slug)] {
			continue
		}
		seen[slug] = true

		name := slug
		imageURL := ""

		if detail, s, e := httpGetJSON(a.thinkific.baseURL+"/api/course_player/v2/courses/"+slug, hdrs); e == nil && s == 200 {
			var dm map[string]interface{}
			if json.Unmarshal(detail, &dm) == nil {
				if co, ok := dm["course"].(map[string]interface{}); ok {
					if n := jsonStr(co, "name"); n != "" {
						name = n
					}
					imageURL = thinkificExtractImage(co)
				} else {
					if n := jsonStr(dm, "name"); n != "" {
						name = n
					}
					imageURL = thinkificExtractImage(dm)
				}
			}
		}

		// Final fallback: scrape og:image from the course HTML page
		if imageURL == "" {
			imageURL = thinkificScrapeOGImage(a.thinkific.baseURL+"/courses/"+slug, hdrs)
		}

		courses = append(courses, ThinkificCourse{
			Slug:       slug,
			Name:       name,
			ImageURL:   imageURL,
			FolderName: sanitizeFilename(name),
		})
	}
	return courses
}

// ── Download Orchestration ─────────────────────────────────────────────────

func (a *App) thinkificDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.thinkificDownloadOne(idx) {
			if a.cancel.Load() {
				break
			}
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}

func (a *App) thinkificDownloadOne(index int) bool {
	if index < 0 || index >= len(a.thinkific.courses) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid course index."})
		return false
	}
	for attempt := 1; attempt <= 3; attempt++ {
		ok, retry := a.thinkificDownloadOneAttempt(index)
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

func (a *App) thinkificDownloadOneAttempt(index int) (ok bool, retryable bool) {
	defer func() {
		if r := recover(); r != nil {
			a.emit("dl_error", map[string]interface{}{
				"index": index, "message": fmt.Sprintf("Error: %v", r),
			})
			ok = false
			retryable = true
		}
	}()

	course := a.thinkific.courses[index]
	hdrs := thinkificHeaders(a.thinkific.cookies)
	hdrs["Referer"] = a.thinkific.baseURL + "/"

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

	// Fetch course data
	data, status, err := httpGetJSON(a.thinkific.baseURL+"/api/course_player/v2/courses/"+course.Slug, hdrs)
	if err != nil || status != 200 {
		a.emit("dl_error", map[string]interface{}{
			"index": index, "message": fmt.Sprintf("Failed to fetch course (HTTP %d)", status),
		})
		return false, true
	}

	var courseData map[string]interface{}
	if json.Unmarshal(data, &courseData) != nil {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Failed to parse course data"})
		return false, true
	}

	courseObj := jsonMap(courseData, "course")
	contents := jsonArray(courseData, "contents")
	chapters := jsonArray(courseData, "chapters")

	// Build chapter-id → name map
	chMap := map[float64]string{}
	for _, ch := range chapters {
		if cm, ok2 := ch.(map[string]interface{}); ok2 {
			if id, ok3 := cm["id"].(float64); ok3 {
				chMap[id] = jsonStr(cm, "name")
			}
		}
	}

	// Download cover image
	coverURL := ""
	if courseObj != nil {
		coverURL = jsonStr(courseObj, "logo")
	}
	if coverURL == "" {
		if pageData, s, e := httpGetJSON(a.thinkific.baseURL+"/courses/"+course.Slug, hdrs); e == nil && s == 200 {
			if m := thinkificCoverOGRe.FindSubmatch(pageData); len(m) > 1 {
				coverURL = string(m[1])
			}
		}
	}
	if coverURL != "" {
		ext := "jpg"
		if parts := strings.Split(strings.Split(coverURL, "?")[0], "."); len(parts) > 1 {
			if e := parts[len(parts)-1]; len(e) <= 4 {
				ext = e
			}
		}
		coverPath := filepath.Join(courseDir, "cover."+ext)
		if _, err2 := os.Stat(coverPath); os.IsNotExist(err2) {
			downloadToFileSimple(coverURL, coverPath, hdrs)
		}
	}

	// Track chapter index for folder numbering
	chapterOrder := map[float64]int{}
	for _, ch := range chapters {
		if cm2, ok2 := ch.(map[string]interface{}); ok2 {
			if id, ok3 := cm2["id"].(float64); ok3 {
				// position field (1-based), fallback to insertion order
				pos := int(id) // will be overwritten below
				if p, ok4 := cm2["position"].(float64); ok4 {
					pos = int(p)
				}
				chapterOrder[id] = pos
			}
		}
	}
	// If positions are all 0 or missing, assign sequential order
	chSeq := map[float64]int{}
	seqIdx := 1
	for _, ch := range chapters {
		if cm2, ok2 := ch.(map[string]interface{}); ok2 {
			if id, ok3 := cm2["id"].(float64); ok3 {
				chSeq[id] = seqIdx
				seqIdx++
			}
		}
	}
	chPad := len(fmt.Sprintf("%d", len(chapters)))
	if chPad < 2 {
		chPad = 2
	}

	total := len(contents) + 1
	current := 1
	pad := len(fmt.Sprintf("%d", len(contents)))
	if pad < 2 {
		pad = 2
	}

	for i, rawContent := range contents {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false, false
		}

		cm, ok2 := rawContent.(map[string]interface{})
		if !ok2 {
			continue
		}

		current++

		lessonName := jsonStr(cm, "name")
		if lessonName == "" {
			lessonName = fmt.Sprintf("Lesson_%d", i+1)
		}

		chID, _ := cm["chapter_id"].(float64)
		chName := chMap[chID]
		if chName == "" {
			chName = "Extras"
		}

		contentableID, _ := cm["contentable_id"].(float64)
		lessonID := int(contentableID)
		ctype := jsonStr(cm, "contentable_type")

		safeName := fmt.Sprintf("%0*d. %s", pad, i+1, sanitizeFilename(lessonName))
		// Number the chapter folder too: "01. Welcome!"
		chIdx := chSeq[chID]
		if chIdx == 0 {
			chIdx = len(chSeq) + 1 // put unknowns at the end
		}
		var chFolderName string
		if chName == "Extras" {
			chFolderName = "Extras"
		} else {
			chFolderName = fmt.Sprintf("%0*d. %s", chPad, chIdx, sanitizeFilename(chName))
		}
		chDir := filepath.Join(courseDir, chFolderName)
		os.MkdirAll(chDir, 0755)

		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": lessonName, "percent": 0,
		})

		if lessonID == 0 {
			continue
		}

		switch ctype {
		case "Download":
			a.thinkificDownloadFiles(lessonID, safeName, chDir, hdrs, index, current, total, lessonName)
		case "HtmlItem":
			a.thinkificSaveHTMLItem(lessonID, safeName, chDir, hdrs)
		default:
			a.thinkificDownloadLesson(lessonID, safeName, chDir, hdrs, index, current, total, lessonName, course.Slug)
		}

		time.Sleep(300 * time.Millisecond)
	}

	markCourseComplete(courseDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": courseDir})
	return true, false
}

// ── Lesson Downloaders ─────────────────────────────────────────────────────

func (a *App) thinkificDownloadLesson(
	lessonID int, safeName, chDir string,
	hdrs map[string]string,
	index, current, total int,
	lessonName, _ string,
) {
	data, status, err := httpGetJSON(
		fmt.Sprintf("%s/api/course_player/v2/lessons/%d", a.thinkific.baseURL, lessonID),
		hdrs,
	)
	if err != nil || status != 200 {
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": lessonName + " (fetch failed)", "percent": 0,
		})
		return
	}

	var lessonData map[string]interface{}
	if json.Unmarshal(data, &lessonData) != nil {
		return
	}

	lesson := jsonMap(lessonData, "lesson")
	if lesson == nil {
		lesson = lessonData
	}
	files := jsonArray(lessonData, "download_files")

	// 1. Save HTML body
	a.thinkificSaveHTML(jsonStr(lesson, "html_text"),
		filepath.Join(chDir, safeName+".html"), lessonName)

	// 2. Download video
	videoURL := jsonStr(lesson, "video_url")
	if videoURL == "" {
		videoURL = jsonStr(lesson, "video_embed_url")
	}
	if videoURL != "" {
		_ = a.thinkificDownloadVideo(videoURL,
			filepath.Join(chDir, safeName+".mp4"),
			index, current, total, lessonName)
	}

	// 3. File attachments
	for _, f := range files {
		fm, ok2 := f.(map[string]interface{})
		if !ok2 {
			continue
		}
		dlURL := jsonStr(fm, "download_url")
		fname := jsonStr(fm, "file_name")
		if fname == "" {
			fname = jsonStr(fm, "label")
		}
		if fname == "" {
			fname = "attachment"
		}
		if dlURL != "" {
			dest := filepath.Join(chDir, sanitizeFilename(fname))
			if _, err2 := os.Stat(dest); os.IsNotExist(err2) {
				downloadToFileSimple(dlURL, dest, hdrs)
			}
		}
	}

	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": lessonName, "percent": 100,
	})
}

func (a *App) thinkificDownloadFiles(
	lessonID int, _ string, chDir string,
	hdrs map[string]string,
	index, current, total int,
	lessonName string,
) {
	data, status, err := httpGetJSON(
		fmt.Sprintf("%s/api/course_player/v2/downloads/%d", a.thinkific.baseURL, lessonID),
		hdrs,
	)
	if err != nil || status != 200 {
		data, status, err = httpGetJSON(
			fmt.Sprintf("%s/api/course_player/v2/lessons/%d", a.thinkific.baseURL, lessonID),
			hdrs,
		)
		if err != nil || status != 200 {
			return
		}
	}

	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) != nil {
		return
	}

	for _, f := range jsonArray(resp, "download_files") {
		fm, ok2 := f.(map[string]interface{})
		if !ok2 {
			continue
		}
		dlURL := jsonStr(fm, "download_url")
		fname := jsonStr(fm, "file_name")
		if fname == "" {
			fname = jsonStr(fm, "label")
		}
		if fname == "" {
			fname = "file"
		}
		if dlURL != "" {
			dest := filepath.Join(chDir, sanitizeFilename(fname))
			if _, err2 := os.Stat(dest); os.IsNotExist(err2) {
				downloadToFileSimple(dlURL, dest, hdrs)
			}
		}
	}

	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": lessonName, "percent": 100,
	})
}

func (a *App) thinkificSaveHTMLItem(
	lessonID int, safeName, chDir string,
	hdrs map[string]string,
) {
	data, status, err := httpGetJSON(
		fmt.Sprintf("%s/api/course_player/v2/html_items/%d", a.thinkific.baseURL, lessonID),
		hdrs,
	)
	if err != nil || status != 200 {
		return
	}
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) != nil {
		return
	}

	htmlItem := jsonMap(resp, "html_item")
	if htmlItem == nil {
		htmlItem = resp
	}

	content := jsonStr(htmlItem, "html_text")
	if content == "" {
		content = jsonStr(htmlItem, "body")
	}
	if content == "" {
		content = jsonStr(htmlItem, "content")
	}

	a.thinkificSaveHTML(content, filepath.Join(chDir, safeName+".html"), safeName)
}

// ── Video Download ─────────────────────────────────────────────────────────

var wistiaIDRe = regexp.MustCompile(
	`(?i)(?:wistia_async_|fast\.wistia\.com/embed/(?:medias|iframe)/|hashedId["']?\s*[:=]\s*["'])([a-z0-9]+)`)

var thinkificCDNRe = regexp.MustCompile(
	`(https://(?:d2p6ecj15pyavq|d1fto35gcfffzn)\.cloudfront\.net/[^\s"']+\.(?:mp4|m3u8)|` +
		`https://player-api\.thinkific\.com/(?:hls|api/video)/[^\s"']+)`)

func (a *App) thinkificDownloadVideo(
	videoURL, outputPath string,
	index, current, total int,
	lessonName string,
) error {
	if _, err := os.Stat(outputPath); err == nil {
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": lessonName + " (already downloaded)", "percent": 100,
		})
		return nil
	}

	// Fetch the lesson player page to detect the video provider
	pageHdrs := map[string]string{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Accept":     "text/html,*/*",
		"Cookie":     a.thinkific.cookies,
		"Referer":    a.thinkific.baseURL + "/",
	}

	pageData, _, pageErr := httpGetJSON(videoURL, pageHdrs)
	pageText := string(pageData)
	downloadURL := ""

	if pageErr == nil && pageText != "" {
		if m := wistiaIDRe.FindStringSubmatch(pageText); len(m) > 1 {
			downloadURL = "https://fast.wistia.com/embed/iframe/" + m[1]
		}
		if downloadURL == "" {
			if m := thinkificCDNRe.FindStringSubmatch(pageText); len(m) > 1 {
				downloadURL = m[1]
			}
		}
		if downloadURL == "" {
			if m := thinkificPlayerEmbRe.FindStringSubmatch(pageText); len(m) > 1 {
				downloadURL = "https://player.thinkific.com/embed/" + m[1]
			}
		}
		if downloadURL == "" {
			if m := thinkificMp4Re.FindString(pageText); m != "" {
				downloadURL = m
			}
		}
	}
	if downloadURL == "" {
		downloadURL = videoURL
	}

	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": lessonName, "percent": 0,
	})

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

	err := a.runYtdlp(outputPath, downloadURL, progress,
		"--add-headers=Cookie:"+a.thinkific.cookies,
		"--concurrent-fragments", "8",
	)
	if err != nil {
		cleanYtdlpTempFiles(outputPath)
		if a.cancel.Load() || strings.Contains(err.Error(), "cancelled") {
			return err
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": current, "total": total,
			"filename": lessonName + " (video failed)", "percent": 0,
		})
		return err
	}

	a.emit("dl_progress", map[string]interface{}{
		"index": index, "current": current, "total": total,
		"filename": lessonName, "percent": 100,
	})
	return nil
}

// ── HTML Saver ─────────────────────────────────────────────────────────────

func (a *App) thinkificSaveHTML(content, filePath, title string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if _, err := os.Stat(filePath); err == nil {
		return
	}

	doc := fmt.Sprintf(`<!DOCTYPE html>
<html><head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 800px; margin: 40px auto; padding: 0 20px; line-height: 1.6; color: #333; }
h1 { color: #0d242f; border-bottom: 2px solid #0d242f; padding-bottom: 10px; }
img { max-width: 100%%; } a { color: #0066cc; }
</style>
</head><body>
<h1>%s</h1>
%s
</body></html>`,
		html.EscapeString(title),
		html.EscapeString(title),
		content,
	)

	os.WriteFile(filePath, []byte(doc), 0644)
}
