package main

import (
	"os/exec"
	"syscall"
)

// ytdlpSysProcAttr returns macOS-specific process attributes for yt-dlp.
// No special attributes needed on macOS.
func ytdlpSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// openFolderCmd returns the OS command to open a folder in Finder.
func openFolderCmd(path string) *exec.Cmd {
	return exec.Command("open", path)
}
