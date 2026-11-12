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
