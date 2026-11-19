package deej

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omriharel/deej/pkg/deej/util"
	"github.com/thoas/go-funk"
	"go.uber.org/zap"
)

type sessionMap struct {
	deej   *Deej
	logger *zap.SugaredLogger

	m    map[string][]Session
	lock sync.Locker

	sessionFinder SessionFinder

	lastSessionRefresh time.Time
	unmappedSessions   []Session

	outputLock       sync.Mutex
	outputEnabled    map[int]bool
	outputStateKnown map[int]bool
	sliderValues     map[int]float32

	defaultOutputModifier   float32
	alternateOutputModifier float32
}

type processIDSession interface {
	ProcessID() uint32
}

type processOutputRouter interface {
	RouteProcessOutput(uint32, string) error
}

const (
	applicationSliderCount          = 5
	defaultOutputModifierSliderID   = applicationSliderCount
	alternateOutputModifierSliderID = applicationSliderCount + 1

	masterSessionName = "master" // master device volume
	systemSessionName = "system" // system sounds volume
	inputSessionName  = "mic"    // microphone input level

	// some targets need to be transformed before their correct audio sessions can be accessed.
	// this prefix identifies those targets to ensure they don't contradict with another similarly-named process
	specialTargetTransformPrefix = "deej."

	// targets the currently active window (Windows-only, experimental)
	specialTargetCurrentWindow = "current"

	// targets all currently unmapped sessions (experimental)
	specialTargetAllUnmapped = "unmapped"

	// this threshold constant assumes that re-acquiring all sessions is a kind of expensive operation,
	// and needs to be limited in some manner. this value was previously user-configurable through a config
	// key "process_refresh_frequency", but exposing this type of implementation detail seems wrong now
	minTimeBetweenSessionRefreshes = time.Second * 5

	// determines whether the map should be refreshed when a slider moves.
	// this is a bit greedy but allows us to ensure sessions are always re-acquired, which is
	// especially important for process groups (because you can have one ongoing session
	// always preventing lookup of other processes bound to its slider, which forces the user
	// to manually refresh sessions). a cleaner way to do this down the line is by registering to notifications
	// whenever a new session is added, but that's too hard to justify for how easy this solution is
	maxTimeBetweenSessionRefreshes = time.Second * 45
)

// this matches friendly device names (on Windows), e.g. "Headphones (Realtek Audio)"
var deviceSessionKeyPattern = regexp.MustCompile(`^.+ \(.+\)$`)

func newSessionMap(deej *Deej, logger *zap.SugaredLogger, sessionFinder SessionFinder) (*sessionMap, error) {
	logger = logger.Named("sessions")

	m := &sessionMap{
		deej:                    deej,
		logger:                  logger,
		m:                       make(map[string][]Session),
		lock:                    &sync.Mutex{},
		sessionFinder:           sessionFinder,
		outputEnabled:           make(map[int]bool),
		outputStateKnown:        make(map[int]bool),
		sliderValues:            make(map[int]float32),
		defaultOutputModifier:   1,
		alternateOutputModifier: 1,
	}

	logger.Debug("Created session map instance")

	return m, nil
}

func (m *sessionMap) initialize() error {
	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to get all sessions during session map initialization", "error", err)
		return fmt.Errorf("get all sessions during init: %w", err)
	}

	m.setupOnConfigReload()
	m.setupOnSliderMove()
	m.setupOnSliderButtonPress()

	return nil
}

func (m *sessionMap) release() error {
	if err := m.sessionFinder.Release(); err != nil {
		m.logger.Warnw("Failed to release session finder during session map release", "error", err)
		return fmt.Errorf("release session finder during release: %w", err)
	}

	return nil
}

// assumes the session map is clean!
// only call on a new session map or as part of refreshSessions which calls reset
func (m *sessionMap) getAndAddSessions() error {

	// mark that we're refreshing before anything else
	m.lastSessionRefresh = time.Now()
	m.unmappedSessions = nil

	sessions, err := m.sessionFinder.GetAllSessions()
	if err != nil {
		m.logger.Warnw("Failed to get sessions from session finder", "error", err)
		return fmt.Errorf("get sessions from SessionFinder: %w", err)
	}

	for _, session := range sessions {
		m.add(session)

		if !m.sessionMapped(session) {
			m.logger.Debugw("Tracking unmapped session", "session", session)
			m.unmappedSessions = append(m.unmappedSessions, session)
		}
	}

	for sliderID := 0; sliderID < applicationSliderCount; sliderID++ {
		m.outputLock.Lock()
		stateKnown := m.outputStateKnown[sliderID]
		m.outputLock.Unlock()
		if stateKnown {
			m.applySliderOutput(sliderID)
		}
		if runtime.GOOS == "windows" {
			m.reapplySliderValue(sliderID)
		}
	}

	m.logger.Infow("Got all audio sessions successfully", "sessionMap", m)

	return nil
}

func (m *sessionMap) setupOnConfigReload() {
	configReloadedChannel := m.deej.config.SubscribeToChanges()

	go func() {
		for {
			select {
			case <-configReloadedChannel:
				m.logger.Info("Detected config reload, attempting to re-acquire all audio sessions")
				m.refreshSessions(false)
			}
		}
	}()
}

func (m *sessionMap) setupOnSliderMove() {
	sliderEventsChannel := m.deej.serial.SubscribeToSliderMoveEvents()

	go func() {
		for {
			select {
			case event := <-sliderEventsChannel:
				m.handleSliderMoveEvent(event)
			}
		}
	}()
}

func (m *sessionMap) setupOnSliderButtonPress() {
	buttonEventsChannel := m.deej.serial.SubscribeToSliderButtonEvents()

	go func() {
		for {
			select {
			case event := <-buttonEventsChannel:
				if event.ButtonID < 0 || event.ButtonID >= applicationSliderCount {
					continue
				}

				alternateOutput := !event.IsOn
				m.outputLock.Lock()
				m.outputEnabled[event.ButtonID] = alternateOutput
				m.outputStateKnown[event.ButtonID] = true
				m.outputLock.Unlock()

				m.logger.Infow("Updated slider output switch", "sliderID", event.ButtonID, "alternateOutput", alternateOutput)
				m.applySliderOutput(event.ButtonID)
				if runtime.GOOS == "windows" {
					m.reapplySliderValue(event.ButtonID)
				}
			}
		}
	}()
}

func (m *sessionMap) applySliderOutput(sliderID int) {
	if sliderID < 0 || sliderID >= applicationSliderCount {
		return
	}
	config := m.deej.config.snapshot()
	if !strings.EqualFold(config.AudioOutputs.Default, "default") {
		m.logger.Warnw("audio_outputs.default must be 'default'", "value", config.AudioOutputs.Default)
		return
	}
	if config.AudioOutputs.Alternate == "" {
		m.logger.Warn("Configure audio_outputs.alternate before using output switches")
		return
	}

	router, ok := m.sessionFinder.(processOutputRouter)
	if !ok {
		m.logger.Warn("Per-application output routing is unavailable on this platform")
		return
	}

	endpointName := ""
	m.outputLock.Lock()
	if m.outputEnabled[sliderID] {
		endpointName = config.AudioOutputs.Alternate
	}
	m.outputLock.Unlock()

	targets, ok := config.SliderMapping.get(sliderID)
	if !ok {
		return
	}

	processIDs := make(map[uint32]struct{})
	for _, target := range targets {
		for _, resolvedTarget := range m.resolveTarget(target) {
			sessions, ok := m.get(resolvedTarget)
			if !ok {
				continue
			}
			for _, session := range sessions {
				processSession, ok := session.(processIDSession)
				if !ok || processSession.ProcessID() == 0 {
					continue
				}
				processIDs[processSession.ProcessID()] = struct{}{}
			}
		}
	}

	for processID := range processIDs {
		if err := router.RouteProcessOutput(processID, endpointName); err != nil {
			m.logger.Warnw("Failed to route mapped process to audio output",
				"processID", processID,
				"endpoint", endpointName,
				"error", err)
		}
	}
}

// performance: explain why force == true at every such use to avoid unintended forced refresh spams
func (m *sessionMap) refreshSessions(force bool) {

	// make sure enough time passed since the last refresh, unless force is true in which case always clear
	if !force && m.lastSessionRefresh.Add(minTimeBetweenSessionRefreshes).After(time.Now()) {
		return
	}

	// clear and release sessions first
	m.clear()

	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to re-acquire all audio sessions", "error", err)
	} else {
		m.logger.Debug("Re-acquired sessions successfully")
	}
}

// returns true if a session is not currently mapped to any slider, false otherwise
// special sessions (master, system, mic) and device-specific sessions always count as mapped,
// even when absent from the config. this makes sense for every current feature that uses "unmapped sessions"
func (m *sessionMap) sessionMapped(session Session) bool {
	config := m.deej.config.snapshot()

	// count master/system/mic as mapped
	if funk.ContainsString([]string{masterSessionName, systemSessionName, inputSessionName}, session.Key()) {
		return true
	}

	// count device sessions as mapped
	if deviceSessionKeyPattern.MatchString(session.Key()) {
		return true
	}

	matchFound := false

	// look through the actual mappings
	config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		for _, target := range targets {

			// ignore special transforms
			if m.targetHasSpecialTransform(target) {
				continue
			}

			// safe to assume this has a single element because we made sure there's no special transform
			target = m.resolveTarget(target)[0]

			if target == session.Key() {
				matchFound = true
				return
			}
		}
	})

	return matchFound
}

func (m *sessionMap) handleSliderMoveEvent(event SliderMoveEvent) {

	// first of all, ensure our session map isn't moldy
	if m.lastSessionRefresh.Add(maxTimeBetweenSessionRefreshes).Before(time.Now()) {
		m.logger.Debug("Stale session map detected on slider move, refreshing")
		m.refreshSessions(true)
	}

	if runtime.GOOS == "windows" {
		switch event.SliderID {
		case defaultOutputModifierSliderID:
			m.setOutputModifier(false, event.PercentValue)
			return
		case alternateOutputModifierSliderID:
			m.setOutputModifier(true, event.PercentValue)
			return
		}
	}

	if event.SliderID >= 0 && event.SliderID < applicationSliderCount {
		m.outputLock.Lock()
		m.sliderValues[event.SliderID] = event.PercentValue
		m.outputLock.Unlock()
	}

	m.applySliderValue(event.SliderID, event.PercentValue)
}

func (m *sessionMap) setOutputModifier(alternate bool, value float32) {
	m.outputLock.Lock()
	if alternate {
		m.alternateOutputModifier = value
	} else {
		m.defaultOutputModifier = value
	}
	m.outputLock.Unlock()

	m.reapplyOutputSliders()
}

func (m *sessionMap) outputModifierForSlider(sliderID int) float32 {
	if runtime.GOOS != "windows" || sliderID < 0 || sliderID >= applicationSliderCount {
		return 1
	}
	m.outputLock.Lock()
	defer m.outputLock.Unlock()

	if m.outputEnabled[sliderID] {
		return m.alternateOutputModifier
	}
	return m.defaultOutputModifier
}

func (m *sessionMap) reapplyOutputSliders() {
	m.outputLock.Lock()
	values := make(map[int]float32, len(m.sliderValues))
	for sliderID, value := range m.sliderValues {
		values[sliderID] = value
	}
	m.outputLock.Unlock()

	for sliderID, value := range values {
		m.applySliderValue(sliderID, value)
	}
}

func (m *sessionMap) reapplySliderValue(sliderID int) {
	m.outputLock.Lock()
	value, ok := m.sliderValues[sliderID]
	m.outputLock.Unlock()
	if ok {
		m.applySliderValue(sliderID, value)
	}
}

func (m *sessionMap) applySliderValue(sliderID int, value float32) {
	// get the targets mapped to this slider from the config
	targets, ok := m.deej.config.snapshot().SliderMapping.get(sliderID)

	// if slider not found in config, silently ignore
	if !ok {
		return
	}

	targetFound := false
	adjustmentFailed := false

	// for each possible target for this slider...
	for _, target := range targets {

		// resolve the target name by cleaning it up and applying any special transformations.
		// depending on the transformation applied, this can result in more than one target name
		resolvedTargets := m.resolveTarget(target)

		// for each resolved target...
		for _, resolvedTarget := range resolvedTargets {

			// check the map for matching sessions
			sessions, ok := m.get(resolvedTarget)

			// no sessions matching this target - move on
			if !ok {
				continue
			}

			targetFound = true

			// iterate all matching sessions and adjust the volume of each one
			for _, session := range sessions {
				sessionValue := value * m.outputModifierForSession(sliderID, session)
				if session.GetVolume() != sessionValue {
					if err := session.SetVolume(sessionValue); err != nil {
						m.logger.Warnw("Failed to set target session volume", "error", err)
						adjustmentFailed = true
					}
				}
			}
		}
	}

	// if we still haven't found a target or the volume adjustment failed, maybe look for the target again.
	// processes could've opened since the last time this slider moved.
	// if they haven't, the cooldown will take care to not spam it up
	if !targetFound {
		m.refreshSessions(false)
	} else if adjustmentFailed {

		// performance: the reason that forcing a refresh here is okay is that we'll only get here
		// when a session's SetVolume call errored, such as in the case of a stale master session
		// (or another, more catastrophic failure happens)
		m.refreshSessions(true)
	}
}

func (m *sessionMap) outputModifierForSession(sliderID int, session Session) float32 {
	if processSession, ok := session.(processIDSession); ok && processSession.ProcessID() != 0 {
		return m.outputModifierForSlider(sliderID)
	}
	if runtime.GOOS == "windows" && strings.EqualFold(session.Key(), systemSessionName) {
		m.outputLock.Lock()
		defer m.outputLock.Unlock()
		return m.defaultOutputModifier
	}
	return 1
}

func (m *sessionMap) targetHasSpecialTransform(target string) bool {
	return strings.HasPrefix(target, specialTargetTransformPrefix)
}

func (m *sessionMap) resolveTarget(target string) []string {

	// start by ignoring the case
	target = strings.ToLower(target)

	// look for any special targets first, by examining the prefix
	if m.targetHasSpecialTransform(target) {
		return m.applyTargetTransform(strings.TrimPrefix(target, specialTargetTransformPrefix))
	}

	return []string{target}
}

func (m *sessionMap) applyTargetTransform(specialTargetName string) []string {

	// select the transformation based on its name
	switch specialTargetName {

	// get current active window
	case specialTargetCurrentWindow:
		currentWindowProcessNames, err := util.GetCurrentWindowProcessNames()

		// silently ignore errors here, as this is on deej's "hot path" (and it could just mean the user's running linux)
		if err != nil {
			return nil
		}

		// we could have gotten a non-lowercase names from that, so let's ensure we return ones that are lowercase
		for targetIdx, target := range currentWindowProcessNames {
			currentWindowProcessNames[targetIdx] = strings.ToLower(target)
		}

		// remove dupes
		return funk.UniqString(currentWindowProcessNames)

	// get currently unmapped sessions
	case specialTargetAllUnmapped:
		targetKeys := make([]string, len(m.unmappedSessions))
		for sessionIdx, session := range m.unmappedSessions {
			targetKeys[sessionIdx] = session.Key()
		}

		return targetKeys
	}

	return nil
}

func (m *sessionMap) add(value Session) {
	m.lock.Lock()
	defer m.lock.Unlock()

	key := value.Key()

	existing, ok := m.m[key]
	if !ok {
		m.m[key] = []Session{value}
	} else {
		m.m[key] = append(existing, value)
	}
}

func (m *sessionMap) get(key string) ([]Session, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	value, ok := m.m[key]
	return value, ok
}

func (m *sessionMap) availableApplications() []string {
	m.lock.Lock()
	defer m.lock.Unlock()

	applications := make([]string, 0, len(m.m))
	for key := range m.m {
		if strings.HasSuffix(strings.ToLower(key), ".exe") {
			applications = append(applications, key)
		}
	}
	sort.Strings(applications)
	return applications
}

func (m *sessionMap) sessionVolume(key string) (float32, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	sessions, ok := m.m[strings.ToLower(key)]
	if !ok || len(sessions) == 0 {
		return 0, false
	}
	return sessions[0].GetVolume(), true
}

func (m *sessionMap) setSessionVolume(key string, value float32) error {
	m.lock.Lock()
	sessions, ok := m.m[strings.ToLower(key)]
	if !ok || len(sessions) == 0 {
		m.lock.Unlock()
		return fmt.Errorf("audio endpoint %q is unavailable", key)
	}
	var refresh bool
	for _, session := range sessions {
		if err := session.SetVolume(value); err != nil {
			if err == errRefreshSessions {
				refresh = true
				continue
			}
			m.lock.Unlock()
			return err
		}
	}
	m.lock.Unlock()
	if refresh {
		m.refreshSessions(true)
	}
	return nil
}

func (m *sessionMap) clear() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.logger.Debug("Releasing and clearing all audio sessions")

	for key, sessions := range m.m {
		for _, session := range sessions {
			session.Release()
		}

		delete(m.m, key)
	}

	m.logger.Debug("Session map cleared")
}

func (m *sessionMap) String() string {
	m.lock.Lock()
	defer m.lock.Unlock()

	sessionCount := 0

	for _, value := range m.m {
		sessionCount += len(value)
	}

	return fmt.Sprintf("<%d audio sessions>", sessionCount)
}
