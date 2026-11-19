package deej

import (
	"fmt"
	"strconv"
	"strings"
)

func setTopLevelConfigValue(contents, key, value string) string {
	lines := strings.SplitAfter(contents, "\n")
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		trimmed := strings.TrimSpace(body)
		if strings.HasPrefix(body, " ") || !strings.HasPrefix(trimmed, key+":") {
			continue
		}

		comment := ""
		if commentIndex := strings.Index(body, " #"); commentIndex >= 0 {
			comment = body[commentIndex:]
		}
		newline := ""
		if strings.HasSuffix(line, "\n") {
			newline = "\n"
		}
		lines[i] = key + ": " + value + comment + newline
		return strings.Join(lines, "")
	}

	if contents != "" && !strings.HasSuffix(contents, "\n") {
		contents += "\n"
	}
	return contents + key + ": " + value + "\n"
}

func setSliderMappingTarget(contents string, sliderID int, target string) string {
	lines := strings.SplitAfter(contents, "\n")
	header := -1
	sectionEnd := len(lines)
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		if strings.TrimSpace(body) == configKeySliderMapping+":" && !strings.HasPrefix(body, " ") {
			header = i
			continue
		}
		if header >= 0 && body != "" && !strings.HasPrefix(body, " ") {
			sectionEnd = i
			break
		}
	}

	if header == -1 {
		if contents != "" && !strings.HasSuffix(contents, "\n") {
			contents += "\n"
		}
		return contents + configKeySliderMapping + ":\n  " + strconv.Itoa(sliderID) + ": " + strconv.Quote(target) + "\n"
	}

	keyPrefix := "  " + strconv.Itoa(sliderID) + ":"
	for i := header + 1; i < sectionEnd; i++ {
		body := strings.TrimSuffix(lines[i], "\n")
		if !strings.HasPrefix(body, keyPrefix) {
			continue
		}

		comment := ""
		if commentIndex := strings.Index(body, " #"); commentIndex >= 0 {
			comment = body[commentIndex:]
		}
		newline := ""
		if strings.HasSuffix(lines[i], "\n") {
			newline = "\n"
		}
		lines[i] = keyPrefix + " " + strconv.Quote(target) + comment + newline

		for j := i + 1; j < sectionEnd; {
			trimmed := strings.TrimSpace(lines[j])
			if strings.HasPrefix(lines[j], "    -") {
				lines = append(lines[:j], lines[j+1:]...)
				sectionEnd--
				continue
			}
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(lines[j], " ") {
				break
			}
			j++
		}
		return strings.Join(lines, "")
	}

	newLine := "  " + strconv.Itoa(sliderID) + ": " + strconv.Quote(target) + "\n"
	lines = append(lines[:sectionEnd], append([]string{newLine}, lines[sectionEnd:]...)...)
	if sectionEnd == len(lines)-1 && !strings.HasSuffix(lines[sectionEnd-1], "\n") {
		lines[sectionEnd-1] += "\n"
	}
	return strings.Join(lines, "")
}

func updateAudioAlternate(contents, endpoint string) string {
	lines := strings.SplitAfter(contents, "\n")
	audioHeader := -1
	sectionEnd := len(lines)
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		trimmed := strings.TrimSpace(body)
		if trimmed == "audio_outputs:" && !strings.HasPrefix(body, " ") {
			audioHeader = i
			continue
		}
		if audioHeader >= 0 && body != "" && !strings.HasPrefix(body, " ") {
			sectionEnd = i
			break
		}
	}

	if audioHeader == -1 {
		if contents != "" && !strings.HasSuffix(contents, "\n") {
			contents += "\n"
		}
		return contents + "audio_outputs:\n  default: default\n  alternate: " + strconv.Quote(endpoint) + "\n"
	}

	for i := audioHeader + 1; i < sectionEnd; i++ {
		body := strings.TrimSuffix(lines[i], "\n")
		if !strings.HasPrefix(strings.TrimSpace(body), "alternate:") {
			continue
		}
		newline := ""
		if strings.HasSuffix(lines[i], "\n") {
			newline = "\n"
		}
		lines[i] = "  alternate: " + strconv.Quote(endpoint) + newline
		return strings.Join(lines, "")
	}

	line := fmt.Sprintf("  alternate: %s\n", strconv.Quote(endpoint))
	lines = append(lines[:sectionEnd], append([]string{line}, lines[sectionEnd:]...)...)
	return strings.Join(lines, "")
}
