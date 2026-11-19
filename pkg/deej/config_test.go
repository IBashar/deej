package deej

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

func TestPopulateAudioOutputChoicesAndProcessMappings(t *testing.T) {
	userConfig := viper.New()
	userConfig.SetConfigType("yaml")
	configText := `slider_mapping:
  0: mpc-hc64.exe
  1: chrome.exe
  2: spotify.exe
  3:
    - pathofexile_x64.exe
    - rocketleague.exe
  4: discord.exe
audio_outputs:
  default: default
  alternate: "Speakers (Logitech PRO X Wireless Gaming Headset)"
`
	if err := userConfig.ReadConfig(strings.NewReader(configText)); err != nil {
		t.Fatalf("read test config: %v", err)
	}

	config := &CanonicalConfig{
		logger:         zap.NewNop().Sugar(),
		userConfig:     userConfig,
		internalConfig: viper.New(),
	}
	if err := config.populateFromVipers(); err != nil {
		t.Fatalf("populate config: %v", err)
	}

	if config.AudioOutputs.Default != "default" {
		t.Fatalf("unexpected default output: %q", config.AudioOutputs.Default)
	}
	if config.AudioOutputs.Alternate != "Speakers (Logitech PRO X Wireless Gaming Headset)" {
		t.Fatalf("unexpected alternate output: %q", config.AudioOutputs.Alternate)
	}
	for sliderID := 0; sliderID < 5; sliderID++ {
		if _, ok := config.SliderMapping.get(sliderID); !ok {
			t.Errorf("missing process mapping for slider %d", sliderID)
		}
	}

}

func TestSetSliderMappingTargetReplacesListAndPreservesOtherConfig(t *testing.T) {
	config := "slider_mapping:\n  0: master\n  1:\n    - old.exe\n    - other.exe\n# keep me\ncom_port: COM5\n"

	updated := setSliderMappingTarget(config, 1, "new.exe")
	if !strings.Contains(updated, "  0: master\n") {
		t.Fatal("expected other slider mapping to be preserved")
	}
	if !strings.Contains(updated, "  1: \"new.exe\"\n") {
		t.Fatal("expected selected slider mapping to be replaced")
	}
	if strings.Contains(updated, "old.exe") || strings.Contains(updated, "other.exe") {
		t.Fatal("expected previous targets for selected slider to be removed")
	}
	if !strings.Contains(updated, "# keep me\ncom_port: COM5\n") {
		t.Fatal("expected unrelated comments and settings to be preserved")
	}

	inserted := setSliderMappingTarget("slider_mapping:\n  0: master\n# serial settings\ncom_port: COM5\n", 1, "new.exe")
	if !strings.Contains(inserted, "  1: \"new.exe\"\n# serial settings\ncom_port: COM5\n") {
		t.Fatal("expected a new slider mapping to remain within the mapping section")
	}
}

func TestConfigTextUpdatesPreserveUnrelatedSettings(t *testing.T) {
	config := "com_port: COM5 # serial device\nbaud_rate: 9600\nnoise_reduction: default\naudio_outputs:\n  default: default\n  alternate: \"Old Device\"\n"

	config = setTopLevelConfigValue(config, "com_port", "COM7")
	config = setTopLevelConfigValue(config, "invert_sliders", "true")
	config = updateAudioAlternate(config, "New Device")

	for _, expected := range []string{
		"com_port: COM7 # serial device\n",
		"baud_rate: 9600\n",
		"invert_sliders: true\n",
		"  alternate: \"New Device\"\n",
	} {
		if !strings.Contains(config, expected) {
			t.Errorf("expected config to contain %q", expected)
		}
	}
}
