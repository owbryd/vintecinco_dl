package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Skool API constants.
const (
	skoolAPIBase = "https://api2.skool.com"
	skoolWebBase = "https://www.skool.com"
)

// buildIDRegex extracts the Next.js buildId from the Skool HTML page.
var buildIDRegex = regexp.MustCompile(`"buildId"\s*:\s*"([^"]+)"`)

// SkoolGroup represents a Skool community group (producer).
type SkoolGroup struct {
	Slug       string
	Name       string
	CoverURL   string
	NumCourses int
	FolderName string
}

// SkoolCourse represents a single course within a Skool group (internal use).
type SkoolCourse struct {
	ID         string
	ShortName  string
	Title      string
	NumModules int
}

// SkoolState holds session cookies and cached groups.
type SkoolState struct {
	cookies []*http.Cookie
	buildID string
	groups  []SkoolGroup
}

// stripEmoji removes emoji and non-renderable symbols from a string.
func stripEmoji(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 0xFFFF {
			continue
		}
		if unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// skoolHeaders returns HTTP headers for Skool API/web requests.
func (a *App) skoolHeaders() map[string]string {
	h := map[string]string{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Accept":     "application/json, text/plain, */*",
		"Origin":     skoolWebBase,
		"Referer":    skoolWebBase + "/",
	}
	if len(a.skool.cookies) > 0 {
		var parts []string
		for _, c := range a.skool.cookies {
			parts = append(parts, c.Name+"="+c.Value)
		}
		h["Cookie"] = strings.Join(parts, "; ")
	}
	return h
}

// skoolLogin authenticates with Skool and fetches the user's groups.
func (a *App) skoolLogin(email, password string) map[string]interface{} {
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})

	req, err := http.NewRequest("POST", skoolAPIBase+"/auth/login", strings.NewReader(string(body)))
	if err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Origin", skoolWebBase)
	req.Header.Set("Referer", skoolWebBase+"/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return map[string]interface{}{"success": false, "error": fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}

	a.skool.cookies = resp.Cookies()

	// Fetch buildId from main page
	hdrs := a.skoolHeaders()
	pageData, _, err := httpGetJSON(skoolWebBase, hdrs)
	if err != nil {
		return map[string]interface{}{"success": false, "error": "Failed to get buildId"}
	}
	m := buildIDRegex.FindSubmatch(pageData)
	if m == nil {
		return map[string]interface{}{"success": false, "error": "buildId not found"}
	}
	a.skool.buildID = string(m[1])

	// Fetch groups
	groupData, gstatus, gerr := httpGetJSON(skoolAPIBase+"/self/groups?limit=50&prefs=false", hdrs)
	if gerr != nil || gstatus != 200 {
		return map[string]interface{}{"success": true}
	}

	var groupsParsed map[string]interface{}
	if json.Unmarshal(groupData, &groupsParsed) != nil {
		return map[string]interface{}{"success": true}
	}

	groups := jsonArray(groupsParsed, "groups")
	a.skool.groups = nil
	for _, g := range groups {
		gm, ok := g.(map[string]interface{})
		if !ok {
			continue
		}
		meta := jsonMap(gm, "metadata")
		name := ""
		cover := ""
		numCourses := 0
		if meta != nil {
			name = jsonStr(meta, "display_name")
			cover = jsonStr(meta, "cover_small_url")
			if nc, ok := meta["num_courses"].(float64); ok {
				numCourses = int(nc)
			}
		}
		if name == "" {
			name = jsonStr(gm, "name")
		}
		name = stripEmoji(name)
		a.skool.groups = append(a.skool.groups, SkoolGroup{
			Slug:       jsonStr(gm, "name"),
			Name:       name,
			CoverURL:   cover,
			NumCourses: numCourses,
			FolderName: sanitizeFilename(name),
		})
	}

	return map[string]interface{}{"success": true}
}

// skoolGetCourses returns groups as downloadable items for the frontend.
func (a *App) skoolGetCourses() []map[string]interface{} {
	result := make([]map[string]interface{}, len(a.skool.groups))
	for i, g := range a.skool.groups {
		dlStatus := courseDownloadStatus(filepath.Join(a.platformDir(), g.FolderName))
		result[i] = map[string]interface{}{
			"name":        g.Name,
			"preview_url": g.CoverURL,
			"lessons":     g.NumCourses,
			"downloaded":  dlStatus != "none",
			"dl_status":   dlStatus,
		}
	}
	return result
}

// skoolFetchGroupCourses fetches all accessible courses within a group.
func (a *App) skoolFetchGroupCourses(slug string) []SkoolCourse {
	hdrs := a.skoolHeaders()
	url := fmt.Sprintf("%s/_next/data/%s/%s/classroom.json?group=%s",
		skoolWebBase, a.skool.buildID, slug, slug)

	data, _, err := httpGetJSON(url, hdrs)
	if err != nil {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	pageProps := jsonMap(parsed, "pageProps")
	if pageProps == nil {
		return nil
	}

	allCourses := jsonArray(pageProps, "allCourses")
	var courses []SkoolCourse

	for _, c := range allCourses {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		meta := jsonMap(cm, "metadata")
		if meta == nil {
			continue
		}
		if _, ok := meta["hasAccess"]; !ok {
			continue
		}
		if priv, ok := meta["privacy"].(float64); ok && priv == 2 {
			continue
		}

		title := jsonStr(meta, "title")
		if title == "" {
			title = jsonStr(cm, "name")
		}
		numMods := 0
		if nm, ok := meta["numModules"].(float64); ok {
			numMods = int(nm)
		}

		courses = append(courses, SkoolCourse{
			ID:        jsonStr(cm, "id"),
			ShortName: jsonStr(cm, "name"),
			Title:     title,
			NumModules: numMods,
		})
	}
	return courses
}

// skoolGetModules fetches all modules (lessons) for a course.
func (a *App) skoolGetModules(slug, courseShortName string) []map[string]interface{} {
	hdrs := a.skoolHeaders()
	url := fmt.Sprintf("%s/_next/data/%s/%s/classroom/%s.json?group=%s&course=%s",
		skoolWebBase, a.skool.buildID, slug, courseShortName, slug, courseShortName)

	data, _, err := httpGetJSON(url, hdrs)
	if err != nil {
		return nil
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}

	// Handle Next.js redirect
	pageProps := jsonMap(parsed, "pageProps")
	if pageProps != nil {
		redirect := jsonStr(pageProps, "__N_REDIRECT")
		if redirect != "" {
			mdRegex := regexp.MustCompile(`md=([a-f0-9]+)`)
			mdMatch := mdRegex.FindStringSubmatch(redirect)
			if mdMatch != nil {
				url2 := fmt.Sprintf("%s/_next/data/%s/%s/classroom/%s.json?md=%s&group=%s&course=%s",
					skoolWebBase, a.skool.buildID, slug, courseShortName,
					mdMatch[1], slug, courseShortName)
				data, _, err = httpGetJSON(url2, hdrs)
				if err != nil {
					return nil
				}
				if json.Unmarshal(data, &parsed) != nil {
					return nil
				}
				pageProps = jsonMap(parsed, "pageProps")
			}
		}
	}

	if pageProps == nil {
		return nil
	}

	courseData := jsonMap(pageProps, "course")
	if courseData == nil {
		return nil
	}

	children := jsonArray(courseData, "children")
	var modules []map[string]interface{}

	// The course name is used as the default section for standalone modules
	courseMeta := jsonMap(jsonMap(courseData, "course"), "metadata")
	courseTitle := "Course"
	if courseMeta != nil {
		if ct := jsonStr(courseMeta, "title"); ct != "" {
			courseTitle = ct
		}
	}

	for _, section := range children {
		sm, ok := section.(map[string]interface{})
		if !ok {
			continue
		}
		sectionCourse := jsonMap(sm, "course")
		if sectionCourse == nil {
			continue
		}

		unitType := jsonStr(sectionCourse, "unitType")
		sectionMeta := jsonMap(sectionCourse, "metadata")

		if unitType == "module" {
			// Standalone module (direct child of course, not inside a set)
			if sectionMeta == nil {
				continue
			}
			modules = append(modules, map[string]interface{}{
				"section":   courseTitle,
				"title":     jsonStr(sectionMeta, "title"),
				"videoLink": jsonStr(sectionMeta, "videoLink"),
				"desc":      jsonStr(sectionMeta, "desc"),
				"resources": jsonStr(sectionMeta, "resources"),
				"moduleId":  jsonStr(sectionCourse, "id"),
			})
			continue
		}

		// unitType == "set": section with sub-lessons
		sectionTitle := "Section"
		if sectionMeta != nil {
			if t := jsonStr(sectionMeta, "title"); t != "" {
				sectionTitle = t
			}
		}

		sectionChildren := jsonArray(sm, "children")
		for _, mod := range sectionChildren {
			mm, ok := mod.(map[string]interface{})
			if !ok {
				continue
			}
			modCourse := jsonMap(mm, "course")
			if modCourse == nil {
				continue
			}
			if jsonStr(modCourse, "unitType") != "module" {
				continue
			}
			modMeta := jsonMap(modCourse, "metadata")
			if modMeta == nil {
				continue
			}

			modules = append(modules, map[string]interface{}{
				"section":   sectionTitle,
				"title":     jsonStr(modMeta, "title"),
				"videoLink": jsonStr(modMeta, "videoLink"),
				"desc":      jsonStr(modMeta, "desc"),
				"resources": jsonStr(modMeta, "resources"),
				"moduleId":  jsonStr(modCourse, "id"),
			})
		}
	}

	return modules
}

// skoolFetchModuleDesc fetches the description for a single module by requesting
// its page individually (the listing only includes desc for the selected module).
func (a *App) skoolFetchModuleDesc(slug, courseShortName, moduleID string) string {
	hdrs := a.skoolHeaders()
	url := fmt.Sprintf("%s/_next/data/%s/%s/classroom/%s.json?md=%s&group=%s&course=%s",
		skoolWebBase, a.skool.buildID, slug, courseShortName,
		moduleID, slug, courseShortName)

	data, _, err := httpGetJSON(url, hdrs)
	if err != nil {
		return ""
	}

	var parsed map[string]interface{}
	if json.Unmarshal(data, &parsed) != nil {
		return ""
	}

	pageProps := jsonMap(parsed, "pageProps")
	if pageProps == nil {
		return ""
	}
	courseData := jsonMap(pageProps, "course")
	if courseData == nil {
		return ""
	}

	// Find the module in children (it should now have desc populated)
	var findDesc func(children []interface{}) string
	findDesc = func(children []interface{}) string {
		for _, child := range children {
			cm, _ := child.(map[string]interface{})
			if cm == nil {
				continue
			}
			cc := jsonMap(cm, "course")
			if cc != nil && jsonStr(cc, "id") == moduleID {
				meta := jsonMap(cc, "metadata")
				if meta != nil {
					return jsonStr(meta, "desc")
				}
			}
			// Check nested children (inside sets)
			if sub := jsonArray(cm, "children"); len(sub) > 0 {
				if d := findDesc(sub); d != "" {
					return d
				}
			}
		}
		return ""
	}

	return findDesc(jsonArray(courseData, "children"))
}

// skoolDownloadOne downloads all courses from a single Skool group.
func (a *App) skoolDownloadOne(index int) bool {
	if index < 0 || index >= len(a.skool.groups) {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "Invalid group."})
		return false
	}

	group := a.skool.groups[index]
	groupDir := filepath.Join(a.platformDir(), group.FolderName)
	os.MkdirAll(groupDir, 0755)

	a.emit("dl_started", map[string]interface{}{"index": index, "folder": groupDir})

	// Fetch courses for this group
	courses := a.skoolFetchGroupCourses(group.Slug)
	if len(courses) == 0 {
		a.emit("dl_error", map[string]interface{}{"index": index, "message": "No courses found in group."})
		return false
	}

	// Fetch all modules and count total lessons for progress
	type courseModules struct {
		title     string
		shortName string
		courseNum int
		modules   []map[string]interface{}
	}

	var allCourses []courseModules
	totalLessons := 0

	for ci, course := range courses {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false
		}
		a.emit("dl_progress", map[string]interface{}{
			"index": index, "current": 0, "total": 0,
			"filename": fmt.Sprintf("Loading course %d/%d: %s", ci+1, len(courses), course.Title),
			"percent": 0,
		})
		modules := a.skoolGetModules(group.Slug, course.ShortName)
		allCourses = append(allCourses, courseModules{
			title:     course.Title,
			shortName: course.ShortName,
			courseNum: ci + 1,
			modules:   modules,
		})
		totalLessons += len(modules)
	}

	currentLesson := 0

	for _, cm := range allCourses {
		if a.cancel.Load() {
			a.emit("dl_cancelled", map[string]interface{}{"index": index})
			return false
		}

		courseDir := filepath.Join(groupDir, sanitizeFilename(fmt.Sprintf("%02d - %s", cm.courseNum, cm.title)))
		currentSection := ""
		sectionNum := 0
		lessonNum := 0

		for _, mod := range cm.modules {
			if a.cancel.Load() {
				a.emit("dl_cancelled", map[string]interface{}{"index": index})
				return false
			}

			section := mod["section"].(string)
			title := mod["title"].(string)
			videoLink, _ := mod["videoLink"].(string)
			desc, _ := mod["desc"].(string)
			resourcesStr, _ := mod["resources"].(string)

			if section != currentSection {
				currentSection = section
				sectionNum++
				lessonNum = 0
			}
			lessonNum++
			currentLesson++

			sectionDir := filepath.Join(courseDir, sanitizeFilename(fmt.Sprintf("%02d - %s", sectionNum, section)))
			lessonDir := filepath.Join(sectionDir, sanitizeFilename(fmt.Sprintf("%02d - %s", lessonNum, title)))
			os.MkdirAll(lessonDir, 0755)

			a.emit("dl_progress", map[string]interface{}{
				"index": index, "current": currentLesson, "total": totalLessons,
				"filename": title, "percent": 0,
			})

			// Fetch description if not in listing data
			if (desc == "" || desc == "false") {
				if mid, _ := mod["moduleId"].(string); mid != "" {
					desc = a.skoolFetchModuleDesc(group.Slug, cm.shortName, mid)
				}
			}

			// Save description as HTML
			if desc != "" && desc != "[v2][{\"type\":\"paragraph\"}]" {
				descPath := filepath.Join(lessonDir, "description.html")
				if _, err := os.Stat(descPath); os.IsNotExist(err) {
					html := skoolDescToHTML(desc)
					os.WriteFile(descPath, []byte(html), 0644)
				}
			}

			// Download video
			if videoLink != "" {
				if !videoExists(lessonDir) && !skoolVideoExists(lessonDir, title) {
					cleanURL := videoLink
					if strings.Contains(videoLink, "?") {
						cleanURL = strings.Split(videoLink, "?")[0]
					}
					if strings.Contains(videoLink, "vimeo.com") {
						cleanURL = strings.Split(videoLink, "?")[0]
					}

					outputTemplate := filepath.Join(lessonDir, sanitizeFilename(title)+".%(ext)s")
					progress := func(dl, tb int64) {
						pct := 50.0
						if tb > 0 {
							pct = float64(dl) / float64(tb) * 100.0
						}
						a.emit("dl_progress", map[string]interface{}{
							"index": index, "current": currentLesson, "total": totalLessons,
							"filename": title, "percent": pct,
						})
					}

					extraArgs := []string{}
					if strings.Contains(videoLink, "vimeo.com") {
						extraArgs = append(extraArgs, "--referer", skoolWebBase+"/")
					}
					if err := a.runYtdlp(outputTemplate, cleanURL, progress, extraArgs...); err != nil {
						cleanYtdlpTempFiles(outputTemplate)
					}
				}
			}

			// Download resources
			if resourcesStr != "" {
				var resources []map[string]interface{}
				if json.Unmarshal([]byte(resourcesStr), &resources) == nil {
					for _, res := range resources {
						resTitle, _ := res["title"].(string)
						resLink, _ := res["link"].(string)
						if resLink == "" || resTitle == "" {
							continue
						}

						if strings.HasPrefix(resLink, "http") {
							ext := filepath.Ext(strings.Split(resLink, "?")[0])
							downloadExts := map[string]bool{
								".pdf": true, ".zip": true, ".doc": true, ".docx": true,
								".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
								".png": true, ".jpg": true, ".mp3": true,
							}
							if downloadExts[strings.ToLower(ext)] {
								filePath := filepath.Join(lessonDir, sanitizeFilename(resTitle)+ext)
								if _, err := os.Stat(filePath); os.IsNotExist(err) {
									downloadToFileSimple(resLink, filePath, map[string]string{
										"User-Agent": "Mozilla/5.0",
									})
								}
								continue
							}
						}

						resPath := filepath.Join(lessonDir, sanitizeFilename(resTitle)+".txt")
						if _, err := os.Stat(resPath); os.IsNotExist(err) {
							os.WriteFile(resPath, []byte(resTitle+"\n"+resLink+"\n"), 0644)
						}
					}
				}
			}
		}
	}

	markCourseComplete(groupDir)
	a.emit("dl_complete", map[string]interface{}{"index": index, "folder": groupDir})
	return true
}

// skoolDescToHTML converts a Skool [v2] rich-text description to simple HTML.
// The format is a TipTap/ProseMirror JSON array prefixed with "[v2]".
func skoolDescToHTML(desc string) string {
	if !strings.HasPrefix(desc, "[v2]") {
		// Plain text — wrap in paragraphs
		return "<p>" + strings.ReplaceAll(desc, "\n", "</p><p>") + "</p>"
	}
	raw := desc[4:]
	var nodes []interface{}
	if json.Unmarshal([]byte(raw), &nodes) != nil {
		return desc
	}
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><meta charset=\"utf-8\"></head><body>\n")
	for _, n := range nodes {
		nm, _ := n.(map[string]interface{})
		if nm == nil {
			continue
		}
		skoolRenderNode(&b, nm)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func skoolRenderNode(b *strings.Builder, node map[string]interface{}) {
	ntype := jsonStr(node, "type")
	content := jsonArray(node, "content")

	switch ntype {
	case "paragraph":
		b.WriteString("<p>")
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
		b.WriteString("</p>\n")
	case "heading":
		level := "2"
		if l, ok := node["attrs"].(map[string]interface{}); ok {
			if lv, ok := l["level"].(float64); ok {
				level = fmt.Sprintf("%d", int(lv))
			}
		}
		b.WriteString("<h" + level + ">")
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
		b.WriteString("</h" + level + ">\n")
	case "text":
		text := jsonStr(node, "text")
		marks := jsonArray(node, "marks")
		for _, m := range marks {
			if mm, ok := m.(map[string]interface{}); ok {
				switch jsonStr(mm, "type") {
				case "bold":
					text = "<b>" + text + "</b>"
				case "italic":
					text = "<i>" + text + "</i>"
				case "link":
					attrs, _ := mm["attrs"].(map[string]interface{})
					href := jsonStr(attrs, "href")
					if href != "" {
						text = "<a href=\"" + href + "\">" + text + "</a>"
					}
				}
			}
		}
		b.WriteString(text)
	case "unorderedList", "bulletList":
		b.WriteString("<ul>\n")
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
		b.WriteString("</ul>\n")
	case "orderedList":
		b.WriteString("<ol>\n")
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
		b.WriteString("</ol>\n")
	case "listItem":
		b.WriteString("<li>")
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
		b.WriteString("</li>\n")
	case "image":
		attrs, _ := node["attrs"].(map[string]interface{})
		src := jsonStr(attrs, "src")
		if src != "" {
			b.WriteString("<img src=\"" + src + "\">\n")
		}
	case "blockquote":
		b.WriteString("<blockquote>")
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
		b.WriteString("</blockquote>\n")
	default:
		for _, c := range content {
			if cm, ok := c.(map[string]interface{}); ok {
				skoolRenderNode(b, cm)
			}
		}
	}
}

// skoolVideoExists checks if a video file with the given title already exists.
func skoolVideoExists(dir, title string) bool {
	sanitized := sanitizeFilename(title)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			name := e.Name()
			ext := filepath.Ext(name)
			base := strings.TrimSuffix(name, ext)
			if base == sanitized && (ext == ".mp4" || ext == ".mkv" || ext == ".webm") {
				return true
			}
		}
	}
	return false
}

// skoolDownloadBatch downloads multiple Skool groups sequentially.
func (a *App) skoolDownloadBatch(indices []int) {
	a.emit("batch_started", map[string]interface{}{"count": len(indices)})
	for qi, idx := range indices {
		if a.cancel.Load() {
			break
		}
		a.emit("batch_progress", map[string]interface{}{
			"queue_pos": qi, "queue_total": len(indices),
		})
		if !a.skoolDownloadOne(idx) {
			break
		}
	}
	a.emit("batch_done", map[string]interface{}{})
}
