package deej

func (d *Deej) startUIStateMonitor() {
	sliderEvents := d.serial.SubscribeToSliderMoveEvents()
	buttonEvents := d.serial.SubscribeToSliderButtonEvents()

	go func() {
		for {
			select {
			case event := <-sliderEvents:
				d.uiStateLock.Lock()
				for len(d.uiSliderValues) <= event.SliderID {
					d.uiSliderValues = append(d.uiSliderValues, -1)
				}
				d.uiSliderValues[event.SliderID] = event.PercentValue
				d.uiStateLock.Unlock()
			case event := <-buttonEvents:
				d.uiStateLock.Lock()
				for len(d.uiButtonStates) <= event.ButtonID {
					d.uiButtonStates = append(d.uiButtonStates, true)
				}
				d.uiButtonStates[event.ButtonID] = event.IsOn
				d.uiButtonKnown = true
				d.uiStateLock.Unlock()
			case <-d.uiStopChannel:
				return
			}
		}
	}()
}

func (d *Deej) uiStateSnapshot() ([]float32, []bool, bool) {
	d.uiStateLock.RLock()
	defer d.uiStateLock.RUnlock()

	sliders := append([]float32(nil), d.uiSliderValues...)
	buttons := append([]bool(nil), d.uiButtonStates...)
	return sliders, buttons, d.uiButtonKnown
}
