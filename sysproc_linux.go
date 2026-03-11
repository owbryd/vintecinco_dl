package main

import (
	"os/exec"
	"syscall"
)

// ytdlpSysProcAttr returns Linux-specific process attributes for yt-dlp.
// No special attributes needed on Linux.
func ytdlpSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// openFolderCmd returns the OS command to open a folder in the file manager.
func openFolderCmd(path string) *exec.Cmd {
	return exec.Command("xdg-open", path)
}
