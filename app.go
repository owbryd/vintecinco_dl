package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the main application struct, bound to the Wails frontend.
// It holds per-platform authentication state, the active download directory,
// and cancellation controls shared across goroutines.
type App struct {
	ctx            context.Context // Wails runtime context, set on startup
	activePlatform string          // currently selected platform ("kiwify", "gumroad", etc.)
	downloadDir    string          // root folder where courses are saved
	cancel         atomic.Bool     // signals running downloads to stop
	ytdlpCmd       *exec.Cmd      // current yt-dlp process, if any (for kill on cancel)
	mu             sync.Mutex     // protects ytdlpCmd

	kiwify      KiwifyState
	gumroad     GumroadState
	hotmart     HotmartState
	teachable   TeachableState
	kajabi      KajabiState
	skool        SkoolState
	pluralsight  PluralSightState
	greatcourses GreatCoursesState
	masterclass  MasterClassState
	thinkific    ThinkificState
}

// NewApp creates a fresh App with zero-value state (no platform selected).
func NewApp() *App {
	return &App{}
}

// startup is called by Wails when the application window is ready.
// It stores the runtime context, injects the bundled-tools bin dir into PATH,
// and sets the default download directory.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	injectBinDir() // make auto-downloaded yt-dlp/ffmpeg visible to exec.LookPath
	a.downloadDir = defaultDownloadDir()
}

// SelectPlatform sets the active platform and returns the current config
// to the frontend (platform id + download folder path).
func (a *App) SelectPlatform(id string) map[string]interface{} {
	a.activePlatform = id
	return map[string]interface{}{
		"platform": id,
		"folder":   a.downloadDir,
	}
}

// GoBack cancels any running download and clears the active platform,
// returning the user to the platform selection screen.
func (a *App) GoBack() {
	a.Cancel()
	a.activePlatform = ""
}

// Login dispatches authentication to the active platform's login handler.
// Returns a map with "success" (bool) and optionally "error" (string)
// or "otp_sent" (bool) for platforms that use OTP (Teachable, Kajabi).
func (a *App) Login(email, password string) map[string]interface{} {
	switch a.activePlatform {
	case "kiwify":
		return a.kiwifyLogin(email, password)
	case "gumroad":
		return a.gumroadLogin(email, password)
	case "hotmart":
		return a.hotmartLogin(email, password)
	case "teachable":
		return a.teachableLogin(email, password)
	case "kajabi":
		return a.kajabiLogin(email, password)
	case "skool":
		return a.skoolLogin(email, password)
	case "pluralsight":
		return a.pluralsightLogin(email, password)
	case "greatcourses":
		return a.greatcoursesLogin(email, password)
	case "masterclass":
		return a.masterclassLogin(email, password)
	case "thinkific":
		return a.thinkificLogin(email, password)
	}
	return map[string]interface{}{"success": false, "error": "Unknown platform"}
}

// GetCourses fetches the list of purchased courses for the active platform.
// Each entry contains name, preview image, and download status.
func (a *App) GetCourses() []map[string]interface{} {
	switch a.activePlatform {
	case "kiwify":
		return a.kiwifyGetCourses()
	case "gumroad":
		return a.gumroadGetCourses()
	case "hotmart":
		return a.hotmartGetCourses()
	case "teachable":
		return a.teachableGetCourses()
	case "kajabi":
		return a.kajabiGetCourses()
	case "skool":
		return a.skoolGetCourses()
	case "pluralsight":
		return a.pluralsightGetCourses()
	case "greatcourses":
		return a.greatcoursesGetCourses()
	case "masterclass":
		return a.masterclassGetCourses()
	case "thinkific":
		return a.thinkificGetCourses()
	}
	return nil
}

// Download starts downloading the courses at the given indices in a background
// goroutine. The cancel flag is reset so previous cancellations don't interfere.
// Progress is reported to the frontend via Wails events.
func (a *App) Download(indices []int) {
	a.cancel.Store(false)
	switch a.activePlatform {
	case "kiwify":
		go a.kiwifyDownloadBatch(indices)
	case "gumroad":
		go a.gumroadDownloadBatch(indices)
	case "hotmart":
		go a.hotmartDownloadBatch(indices)
	case "teachable":
		go a.teachableDownloadBatch(indices)
	case "kajabi":
		go a.kajabiDownloadBatch(indices)
	case "skool":
		go a.skoolDownloadBatch(indices)
	case "pluralsight":
		go a.pluralsightDownloadBatch(indices)
	case "greatcourses":
		go a.greatcoursesDownloadBatch(indices)
	case "masterclass":
		go a.masterclassDownloadBatch(indices)
	case "thinkific":
		go a.thinkificDownloadBatch(indices)
	}
}

// Cancel signals all running downloads to stop and kills any active yt-dlp process.
func (a *App) Cancel() {
	a.cancel.Store(true)
	a.mu.Lock()
	if a.ytdlpCmd != nil && a.ytdlpCmd.Process != nil {
		a.ytdlpCmd.Process.Kill()
	}
	a.mu.Unlock()
}

// PickFolder opens a native directory picker dialog and updates the download
// directory. Returns the selected (or unchanged) path.
func (a *App) PickFolder() string {
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Select download folder",
		DefaultDirectory: a.downloadDir,
	})
	if err != nil || dir == "" {
		return a.downloadDir
	}
	a.downloadDir = dir
	return dir
}

// courseFolderName returns the filesystem folder name for the course at index,
// or "" if the index is out of bounds or the platform is unknown.
func (a *App) courseFolderName(index int) string {
	switch a.activePlatform {
	case "kiwify":
		if index >= 0 && index < len(a.kiwify.courses) {
			return a.kiwify.courses[index].FolderName
		}
	case "gumroad":
		if index >= 0 && index < len(a.gumroad.courses) {
			return a.gumroad.courses[index].FolderName
		}
	case "hotmart":
		if index >= 0 && index < len(a.hotmart.courses) {
			return a.hotmart.courses[index].FolderName
		}
	case "teachable":
		if index >= 0 && index < len(a.teachable.courses) {
			return a.teachable.courses[index].FolderName
		}
	case "kajabi":
		if index >= 0 && index < len(a.kajabi.courses) {
			return a.kajabi.courses[index].FolderName
		}
	case "skool":
		if index >= 0 && index < len(a.skool.groups) {
			return a.skool.groups[index].FolderName
		}
	case "pluralsight":
		if index >= 0 && index < len(a.pluralsight.courses) {
			return a.pluralsight.courses[index].FolderName
		}
	case "greatcourses":
		if index >= 0 && index < len(a.greatcourses.courses) {
			return a.greatcourses.courses[index].FolderName
		}
	case "masterclass":
		if index >= 0 && index < len(a.masterclass.courses) {
			return a.masterclass.courses[index].FolderName
		}
	case "thinkific":
		if index >= 0 && index < len(a.thinkific.courses) {
			return a.thinkific.courses[index].FolderName
		}
	}
	return ""
}

// DeleteCourse removes the entire download folder for the course at index.
func (a *App) DeleteCourse(index int) map[string]interface{} {
	folder := a.courseFolderName(index)
	if folder == "" {
		return map[string]interface{}{"success": false, "error": "Invalid index"}
	}
	path := filepath.Join(a.platformDir(), folder)
	if err := os.RemoveAll(path); err != nil {
		return map[string]interface{}{"success": false, "error": err.Error()}
	}
	return map[string]interface{}{"success": true}
}

// OpenFolder opens the given path in the OS file explorer.
func (a *App) OpenFolder(path string) {
	cmd := openFolderCmd(path)
	cmd.Start()
}

// OpenCourseFolder opens the download folder for the course at index.
func (a *App) OpenCourseFolder(index int) {
	if folder := a.courseFolderName(index); folder != "" {
		a.OpenFolder(filepath.Join(a.platformDir(), folder))
	}
}

// Logout cancels downloads and resets authentication state for the active platform.
func (a *App) Logout() {
	a.Cancel()
	switch a.activePlatform {
	case "kiwify":
		a.kiwify = KiwifyState{}
	case "gumroad":
		a.gumroad = GumroadState{}
	case "hotmart":
		a.hotmart = HotmartState{}
	case "teachable":
		a.teachable = TeachableState{}
	case "kajabi":
		a.kajabi = KajabiState{}
	case "skool":
		a.skool = SkoolState{}
	case "pluralsight":
		a.pluralsight = PluralSightState{}
	case "greatcourses":
		a.greatcourses = GreatCoursesState{}
	case "masterclass":
		a.masterclass = MasterClassState{}
	case "thinkific":
		a.thinkific = ThinkificState{}
	}
}

// GetDownloadDir returns the current download directory path.
func (a *App) GetDownloadDir() string {
	return a.downloadDir
}

// CheckDeps reports whether yt-dlp and ffmpeg are available.
// Checks the bundled bin dir first (stat), then falls back to PATH lookup.
// Called by the frontend on startup to decide whether to show the setup screen.
func (a *App) CheckDeps() map[string]bool {
	return map[string]bool{
		"ytdlp":  depsHas("yt-dlp", ytdlpBin()),
		"ffmpeg": depsHas("ffmpeg", ffmpegBin()),
	}
}

// depsHas returns true if the tool exists in depsBinDir() OR in the system PATH.
func depsHas(name, binFile string) bool {
	if _, err := os.Stat(filepath.Join(depsBinDir(), binFile)); err == nil {
		return true
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// SelectSubgroup selects a school (Teachable) or site (Kajabi) by index.
// These platforms group courses under schools/sites, so one must be selected
// before listing courses.
func (a *App) SelectSubgroup(index int) map[string]interface{} {
	switch a.activePlatform {
	case "teachable":
		if index >= 0 && index < len(a.teachable.schools) {
			a.teachable.schoolID = jsonStr(a.teachable.schools[index], "id")
			return map[string]interface{}{"success": true}
		}
	case "kajabi":
		if index >= 0 && index < len(a.kajabi.sites) {
			a.kajabi.siteID = jsonStr(a.kajabi.sites[index], "id")
			return map[string]interface{}{"success": true}
		}
	case "skool":
		return map[string]interface{}{"success": false}
	}
	return map[string]interface{}{"success": false}
}

// platformDir returns the platform-specific subdirectory inside downloadDir.
// Each downloader saves into its own folder (e.g. vintecinco_dl/Hotmart/).
func (a *App) platformDir() string {
	sub := ""
	switch a.activePlatform {
	case "kiwify":
		sub = "Kiwify"
	case "gumroad":
		sub = "Gumroad"
	case "hotmart":
		sub = "Hotmart"
	case "teachable":
		sub = "Teachable"
	case "kajabi":
		sub = "Kajabi"
	case "skool":
		sub = "Skool"
	case "pluralsight":
		sub = "Pluralsight"
	case "greatcourses":
		sub = "The Great Courses"
	case "masterclass":
		sub = "MasterClass"
	case "thinkific":
		sub = "Thinkific"
	}
	if sub == "" {
		return a.downloadDir
	}
	return filepath.Join(a.downloadDir, sub)
}

// emit sends an event to the Wails frontend with the given name and data payload.
// Common events: dl_started, dl_progress, dl_complete, dl_cancelled, dl_error,
// batch_started, batch_progress, batch_done.
func (a *App) emit(event string, data map[string]interface{}) {
	runtime.EventsEmit(a.ctx, event, data)
}

// sleepMs pauses execution for the given number of milliseconds.
func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// defaultDownloadDir returns the default folder for saving courses.
// Prefers ~/Downloads/vintecinco_dl, falls back to ~/vintecinco_dl.
func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "downloads")
	}
	downloads := filepath.Join(home, "Downloads")
	if _, err := os.Stat(downloads); err == nil {
		return filepath.Join(downloads, "vintecinco_dl")
	}
	return filepath.Join(home, "vintecinco_dl")
}
