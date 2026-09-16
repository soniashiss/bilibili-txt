//go:build darwin

package webui

import "os/exec"

func openBrowser(url string, chromeApp bool) error {
	if chromeApp {
		if err := exec.Command("open", "-na", "Google Chrome", "--args", "--app="+url).Start(); err == nil {
			return nil
		}
	}
	return exec.Command("open", url).Start()
}
