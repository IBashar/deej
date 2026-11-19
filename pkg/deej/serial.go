package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	comPort  string
	baudRate uint

	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel chan bool
	connected   bool
	connOptions serial.OpenOptions
	conn        io.ReadWriteCloser

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers   []chan SliderMoveEvent
	sliderButtonConsumers []chan SliderButtonEvent
	lastButtonStates      []bool
	buttonStatesReady     bool
	stateLock             sync.RWMutex
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

// SliderButtonEvent represents a button press captured by deej.
type SliderButtonEvent struct {
	ButtonID int
	IsOn     bool
}

var expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*(;[01](\|[01])*)?\r\n$`)

func parseSerialLine(line string) ([]int, []bool, bool) {
	if !expectedLinePattern.MatchString(line) {
		return nil, nil, false
	}

	sections := strings.SplitN(strings.TrimSuffix(line, "\r\n"), ";", 2)
	var sliderValues []int
	for _, value := range strings.Split(sections[0], "|") {
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 || number > 1023 {
			return nil, nil, false
		}
		sliderValues = append(sliderValues, number)
	}

	var buttonStates []bool
	if len(sections) == 2 {
		for _, state := range strings.Split(sections[1], "|") {
			buttonStates = append(buttonStates, state == "1")
		}
	}

	return sliderValues, buttonStates, true
}

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                  deej,
		logger:                logger,
		stopChannel:           make(chan bool),
		connected:             false,
		conn:                  nil,
		sliderMoveConsumers:   []chan SliderMoveEvent{},
		sliderButtonConsumers: []chan SliderButtonEvent{},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to our arduino chip
func (sio *SerialIO) Start() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	// set minimum read size according to platform (0 for windows, 1 for linux)
	// this prevents a rare bug on windows where serial reads get congested,
	// resulting in significant lag
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	config := sio.deej.config.snapshot()
	sio.connOptions = serial.OpenOptions{
		PortName:        config.COMPort,
		BaudRate:        uint(config.BaudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: uint(minimumReadSize),
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", sio.connOptions.PortName,
		"baudRate", sio.connOptions.BaudRate,
		"minReadSize", minimumReadSize)

	var err error
	sio.conn, err = serial.Open(sio.connOptions)
	if err != nil {

		// might need a user notification here, TBD
		sio.logger.Warnw("Failed to open serial connection", "error", err)
		return fmt.Errorf("open serial connection: %w", err)
	}

	namedLogger := sio.logger.Named(strings.ToLower(sio.connOptions.PortName))

	namedLogger.Infow("Connected", "conn", sio.conn)
	sio.connected = true

	// read lines or await a stop
	go func() {
		connReader := bufio.NewReader(sio.conn)
		lineChannel := sio.readLine(namedLogger, connReader)

		for {
			select {
			case <-sio.stopChannel:
				sio.close(namedLogger)
			case line := <-lineChannel:
				sio.handleLine(namedLogger, line)
			}
		}
	}()

	return nil
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.stopChannel <- true
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

// SubscribeToSliderButtonEvents returns a channel that receives a button press event.
func (sio *SerialIO) SubscribeToSliderButtonEvents() chan SliderButtonEvent {
	ch := make(chan SliderButtonEvent)
	sio.sliderButtonConsumers = append(sio.sliderButtonConsumers, ch)

	return ch
}

// SliderState returns the current number of sliders and their normalized values.
func (sio *SerialIO) SliderState() (int, []float32) {
	sio.stateLock.RLock()
	defer sio.stateLock.RUnlock()

	values := append([]float32(nil), sio.currentSliderPercentValues...)
	return len(values), values
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:

				// make any config reload unset our slider number to ensure process volumes are being re-set
				// (the next read line will emit SliderMoveEvent instances for all sliders)\
				// this needs to happen after a small delay, because the session map will also re-acquire sessions
				// whenever the config file is reloaded, and we don't want it to receive these move events while the map
				// is still cleared. this is kind of ugly, but shouldn't cause any issues
				go func() {
					<-time.After(stopDelay)
					sio.lastKnownNumSliders = 0
				}()

				// if connection params have changed, attempt to stop and start the connection
				config := sio.deej.config.snapshot()
				if config.COMPort != sio.connOptions.PortName ||
					uint(config.BaudRate) != sio.connOptions.BaudRate {

					sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
					sio.Stop()

					// let the connection close
					<-time.After(stopDelay)

					if err := sio.Start(); err != nil {
						sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
					} else {
						sio.logger.Debug("Renewed connection successfully")
					}
				}
			}
		}
	}()
}

func (sio *SerialIO) close(logger *zap.SugaredLogger) {
	if err := sio.conn.Close(); err != nil {
		logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		logger.Debug("Serial connection closed")
	}

	sio.conn = nil
	sio.connected = false
}

func (sio *SerialIO) readLine(logger *zap.SugaredLogger, reader *bufio.Reader) chan string {
	ch := make(chan string)

	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {

				if sio.deej.Verbose() {
					logger.Warnw("Failed to read line from serial", "error", err, "line", line)
				}

				// just ignore the line, the read loop will stop after this
				return
			}

			if sio.deej.Verbose() {
				logger.Debugw("Read new line", "line", line)
			}

			// deliver the line to the channel
			ch <- line
		}
	}()

	return ch
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	sliderValues, buttonStates, valid := parseSerialLine(line)
	if !valid {
		return
	}
	numSliders := len(sliderValues)
	config := sio.deej.config.snapshot()

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.stateLock.Lock()
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
		sio.stateLock.Unlock()
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, number := range sliderValues {
		sio.stateLock.RLock()
		currentValue := sio.currentSliderPercentValues[sliderIdx]
		sio.stateLock.RUnlock()

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / 1023.0

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if config.InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(currentValue, normalizedScalar, config.NoiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.stateLock.Lock()
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar
			sio.stateLock.Unlock()

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}

	if buttonStates != nil {
		if len(buttonStates) != len(sio.lastButtonStates) {
			sio.lastButtonStates = make([]bool, len(buttonStates))
			sio.buttonStatesReady = false
		}

		if !sio.buttonStatesReady {
			copy(sio.lastButtonStates, buttonStates)
			sio.buttonStatesReady = true
			for buttonID, isOn := range buttonStates {
				event := SliderButtonEvent{ButtonID: buttonID, IsOn: isOn}
				for _, consumer := range sio.sliderButtonConsumers {
					consumer <- event
				}
			}
			return
		}

		for buttonID, isOn := range buttonStates {
			if isOn != sio.lastButtonStates[buttonID] {
				event := SliderButtonEvent{ButtonID: buttonID, IsOn: isOn}
				for _, consumer := range sio.sliderButtonConsumers {
					consumer <- event
				}
			}
		}
		copy(sio.lastButtonStates, buttonStates)
	}
}
