package deej

import (
	"reflect"
	"testing"

	"go.uber.org/zap"
)

func TestParseSerialLineWithButtons(t *testing.T) {
	values, buttons, valid := parseSerialLine("0|512|1023;0|1|0|1|0\r\n")
	if !valid {
		t.Fatal("expected serial line to be valid")
	}
	if !reflect.DeepEqual(values, []int{0, 512, 1023}) {
		t.Fatalf("unexpected slider values: %v", values)
	}
	if !reflect.DeepEqual(buttons, []bool{false, true, false, true, false}) {
		t.Fatalf("unexpected button states: %v", buttons)
	}
}

func TestParseSerialLineWithoutButtons(t *testing.T) {
	values, buttons, valid := parseSerialLine("12|34|56\r\n")
	if !valid {
		t.Fatal("expected legacy serial line to be valid")
	}
	if !reflect.DeepEqual(values, []int{12, 34, 56}) {
		t.Fatalf("unexpected slider values: %v", values)
	}
	if buttons != nil {
		t.Fatalf("expected no button states, got %v", buttons)
	}
}

func TestParseSerialLineRejectsOutOfRangeSlider(t *testing.T) {
	if _, _, valid := parseSerialLine("1024|0;0|0|0|0|0\r\n"); valid {
		t.Fatal("expected out-of-range slider value to be rejected")
	}
}

func TestHandleLineReportsInitialAndChangedButtonStates(t *testing.T) {
	logger := zap.NewNop().Sugar()
	buttonEvents := make(chan SliderButtonEvent, 10)
	serial := &SerialIO{
		deej:                  &Deej{config: &CanonicalConfig{}},
		logger:                logger,
		sliderButtonConsumers: []chan SliderButtonEvent{buttonEvents},
	}

	serial.handleLine(logger, "0|0;0|1|0|0|0\r\n")
	for buttonID := 0; buttonID < 5; buttonID++ {
		event := <-buttonEvents
		if event.ButtonID != buttonID || event.IsOn != (buttonID == 1) {
			t.Fatalf("unexpected initial button event: %+v", event)
		}
	}

	serial.handleLine(logger, "0|0;0|0|0|0|0\r\n")
	event := <-buttonEvents
	if event.ButtonID != 1 || event.IsOn {
		t.Fatalf("unexpected switch-off event: %+v", event)
	}
	if len(buttonEvents) != 0 {
		t.Fatalf("expected only the changed switch event, got %d events", len(buttonEvents))
	}
}
