package deej

import (
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type modifierTestSession struct {
	key    string
	volume float32
}

func (s *modifierTestSession) GetVolume() float32 {
	return s.volume
}

func (s *modifierTestSession) SetVolume(volume float32) error {
	s.volume = volume
	return nil
}

func (s *modifierTestSession) Key() string {
	return s.key
}

func (s *modifierTestSession) Release() {}

type modifierTestProcessSession struct {
	*modifierTestSession
	pid uint32
}

func (s *modifierTestProcessSession) ProcessID() uint32 {
	return s.pid
}

func TestOutputModifiersScaleApplicationSessionsWithoutChangingEndpointVolumes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("output modifiers are Windows-only")
	}

	mapping := newSliderMap()
	mapping.set(0, []string{"app.exe"})
	mapping.set(1, []string{systemSessionName})
	mapping.set(defaultOutputModifierSliderID, []string{masterSessionName})
	mapping.set(alternateOutputModifierSliderID, []string{"alternate output"})

	config := &CanonicalConfig{
		SliderMapping: mapping,
		AudioOutputs: AudioOutputChoices{
			Default:   "default",
			Alternate: "alternate output",
		},
	}
	app := &modifierTestProcessSession{
		modifierTestSession: &modifierTestSession{key: "app.exe"},
		pid:                 42,
	}
	master := &modifierTestSession{key: masterSessionName, volume: 0.51}
	alternate := &modifierTestSession{key: "alternate output", volume: 0.37}
	system := &modifierTestSession{key: systemSessionName, volume: 0.6}
	sessions := &sessionMap{
		deej:                    &Deej{config: config},
		logger:                  zap.NewNop().Sugar(),
		m:                       map[string][]Session{"app.exe": {app}, masterSessionName: {master}, "alternate output": {alternate}, systemSessionName: {system}},
		lock:                    &sync.Mutex{},
		lastSessionRefresh:      time.Now(),
		outputEnabled:           make(map[int]bool),
		outputStateKnown:        make(map[int]bool),
		sliderValues:            make(map[int]float32),
		defaultOutputModifier:   1,
		alternateOutputModifier: 1,
	}

	sessions.handleSliderMoveEvent(SliderMoveEvent{SliderID: 0, PercentValue: 0.8})
	sessions.handleSliderMoveEvent(SliderMoveEvent{SliderID: 1, PercentValue: 0.8})
	sessions.handleSliderMoveEvent(SliderMoveEvent{SliderID: defaultOutputModifierSliderID, PercentValue: 0})
	assertSessionVolume(t, app, 0)
	assertSessionVolume(t, system, 0)
	assertSessionVolume(t, master, 0.51)

	sessions.outputLock.Lock()
	sessions.outputEnabled[0] = true
	sessions.outputLock.Unlock()
	sessions.handleSliderMoveEvent(SliderMoveEvent{SliderID: alternateOutputModifierSliderID, PercentValue: 0.25})
	assertSessionVolume(t, app, 0.2)
	assertSessionVolume(t, system, 0)
	assertSessionVolume(t, alternate, 0.37)

	sessions.handleSliderMoveEvent(SliderMoveEvent{SliderID: 0, PercentValue: 0.4})
	assertSessionVolume(t, app, 0.1)
}

func assertSessionVolume(t *testing.T, session Session, want float32) {
	t.Helper()
	if got := session.GetVolume(); math.Abs(float64(got-want)) > 0.00001 {
		t.Fatalf("%s volume = %v, want %v", session.Key(), got, want)
	}
}
