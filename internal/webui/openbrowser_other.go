//go:build !darwin

package webui

import (
	"os/exec"
	"runtime"
)

// openBrowser opens url in the user's default browser as a normal tab.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
