package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Pre-compiled regexes for filename sanitization and date parsing.
var (
	invalidCharsRe = regexp.MustCompile(`[<>:"/\\|?*]`)   // characters forbidden in file/folder names
	ampersandRe    = regexp.MustCompile(`&`)               // replaced with "and" for readability
	controlCharsRe = regexp.MustCompile(`[\x00-\x1f]`)    // ASCII control characters (NUL, BEL, TAB, etc.)
	multiSpaceRe   = regexp.MustCompile(`\s+`)             // collapses multiple whitespace
	dateRe         = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`) // matches YYYY-MM-DD
)

// sanitizeFilename cleans a string so it can be used as a safe file or folder name.
// It removes invalid characters, replaces "&" with "and", strips control chars,
// collapses whitespace, and replaces leading dots with underscores.
// Returns "unnamed" for empty/blank results.
func sanitizeFilename(name string) string {
	r := strings.ReplaceAll(name, "\t", " ")
	r = invalidCharsRe.ReplaceAllString(r, "")
	dotCount := 0
	for dotCount < len(r) && r[dotCount] == '.' {
		dotCount++
	}
	if dotCount > 0 {
		r = strings.Repeat("_", dotCount) + r[dotCount:]
	}
	r = ampersandRe.ReplaceAllString(r, "and")
	r = controlCharsRe.ReplaceAllString(r, "")
	r = multiSpaceRe.ReplaceAllString(r, " ")
	r = strings.TrimSpace(r)
	if r == "" {
		return "unnamed"
	}
	return r
}

// formatPurchaseDate converts a date from "YYYY-MM-DD" to "DD-MM-YYYY" format.
// Returns "" if no valid date is found in the input string.
func formatPurchaseDate(dateStr string) string {
	m := dateRe.FindStringSubmatch(dateStr)
	if len(m) == 4 {
		return m[3] + "-" + m[2] + "-" + m[1]
	}
	return ""
}

// makeCourseFolder builds a folder name like "[DD-MM-YYYY] Course Name".
// If purchaseDate is empty or invalid, returns just the sanitized name.
func makeCourseFolder(name, purchaseDate string) string {
	folder := sanitizeFilename(name)
	if purchaseDate != "" {
		fd := formatPurchaseDate(purchaseDate)
		if fd != "" {
			folder = "[" + fd + "] " + folder
		}
	}
	return folder
}

// videoExists checks if a file named "video.*" (any extension) exists in dir.
// Used to detect if a lesson's video has already been downloaded.
func videoExists(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			ext := filepath.Ext(e.Name())
			base := strings.TrimSuffix(e.Name(), ext)
			if base == "video" {
				return true
			}
		}
	}
	return false
}

// lessonAlreadyDone checks if a lesson directory has any meaningful content
// (video, description.html, or attachments). It also cleans up leftover .part
// files from interrupted downloads.
func lessonAlreadyDone(lessonDir string) bool {
	if _, err := os.Stat(lessonDir); os.IsNotExist(err) {
		return false
	}
	cleanPartFiles(lessonDir)
	attachDir := filepath.Join(lessonDir, "Attachments")
	cleanPartFiles(attachDir)

	if videoExists(lessonDir) {
		return true
	}
	if _, err := os.Stat(filepath.Join(lessonDir, "description.html")); err == nil {
		return true
	}
	if entries, err := os.ReadDir(attachDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				return true
			}
		}
	}
	return false
}

// cleanPartFiles removes all .part files (incomplete downloads) from a directory.
func cleanPartFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".part" {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// courseDownloadStatus returns the download state of a course directory:
//   - "none"     — directory does not exist
//   - "complete" — directory contains a .complete marker file
//   - "partial"  — directory exists but download is not finished
func courseDownloadStatus(courseDir string) string {
	if _, err := os.Stat(courseDir); os.IsNotExist(err) {
		return "none"
	}
	if _, err := os.Stat(filepath.Join(courseDir, ".complete")); err == nil {
		return "complete"
	}
	return "partial"
}

// markCourseComplete writes a ".complete" marker file in the course directory
// to signal that all lessons have been downloaded successfully.
func markCourseComplete(courseDir string) {
	os.WriteFile(filepath.Join(courseDir, ".complete"), []byte("done"), 0644)
}

// jsonStr extracts a string value from a JSON-decoded map.
// Also handles float64 values (Go's default for JSON numbers) by converting
// integers to their string representation. Returns "" for missing/nil/other types.
func jsonStr(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	}
	return ""
}

// jsonMap extracts a nested map from a JSON-decoded map.
// Returns nil if the key is missing or the value is not a map.
func jsonMap(m map[string]interface{}, key string) map[string]interface{} {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if mm, ok := v.(map[string]interface{}); ok {
		return mm
	}
	return nil
}

// jsonArray extracts an array from a JSON-decoded map.
// Returns nil if the key is missing or the value is not a slice.
func jsonArray(m map[string]interface{}, key string) []interface{} {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	return nil
}
