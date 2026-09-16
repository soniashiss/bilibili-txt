//go:build !darwin

package webui

import (
	"os/exec"
	"runtime"
)

func openBrowser(url string, _ bool) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
