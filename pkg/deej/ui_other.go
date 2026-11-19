//go:build !windows
// +build !windows

package deej

import "fmt"

func (d *Deej) openWindow() {
	d.logger.Info("The native deej UI is currently available on Windows")
}

func (d *Deej) RunUIPreview() error {
	return fmt.Errorf("the native deej UI preview is only available on Windows")
}
