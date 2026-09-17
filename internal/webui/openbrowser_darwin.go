//go:build darwin

package webui

import "os/exec"

// openBrowser opens url in the user's default browser as a normal tab.
func openBrowser(url string) error {
	return exec.Command("open", url).Start()
}
