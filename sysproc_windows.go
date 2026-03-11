package main

import (
	"os/exec"
	"syscall"
)

// ytdlpSysProcAttr returns Windows-specific process attributes for yt-dlp.
// CREATE_NO_WINDOW (0x08000000) prevents a console window from flashing
// every time yt-dlp is spawned.
func ytdlpSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// openFolderCmd returns the OS command to open a folder in the file explorer.
func openFolderCmd(path string) *exec.Cmd {
	return exec.Command("explorer", path)
}
