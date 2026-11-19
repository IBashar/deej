//go:build windows
// +build windows

package deej

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/lxn/walk"
	ps "github.com/mitchellh/go-ps"
	wca "github.com/moutend/go-wca"
	winapi "golang.org/x/sys/windows"
)

type deejUI struct {
	deej   *Deej
	window *walk.MainWindow
	status *walk.Label

	sliderCards     []*sliderCard
	endpointCards   []*endpointCard
	sliderParent    walk.Container
	processPaths    map[string]string
	icons           map[string]*walk.Icon
	lastIconRefresh time.Time
	defaultOutput   string
	outputChoices   []string
	closed          int32
	stopRefresh     chan struct{}
	stopOnce        sync.Once

	comPortEdit  *walk.LineEdit
	baudRateEdit *walk.LineEdit
	invertCheck  *walk.CheckBox
	noiseCombo   *walk.ComboBox
	settingsNote *walk.Label
}

type sliderCard struct {
	container        *walk.GroupBox
	mapping          *walk.Label
	icon             *walk.ImageView
	displayedIcon    *walk.Icon
	slider           *walk.Slider
	percent          *walk.Label
	button           *walk.Label
	buttonColorState int
}

const (
	buttonColorUnknown  = -1
	buttonColorPressed  = 0
	buttonColorReleased = 1

	windowOwner              = 4
	windowExtendedStyleIndex = 0xFFFFFFEC
	windowExStyleTool        = 0x80
	windowExStyleApp         = 0x40000
)

type endpointCard struct {
	key      string
	sliderID int
	name     *walk.Label
	slider   *walk.Slider
	percent  *walk.Label
}

var (
	queryFullProcessImageNameProc = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
	enumWindowsProc               = syscall.NewLazyDLL("user32.dll").NewProc("EnumWindows")
	isWindowVisibleProc           = syscall.NewLazyDLL("user32.dll").NewProc("IsWindowVisible")
	getWindowTextLengthProc       = syscall.NewLazyDLL("user32.dll").NewProc("GetWindowTextLengthW")
	getWindowProc                 = syscall.NewLazyDLL("user32.dll").NewProc("GetWindow")
	getWindowLongProc             = syscall.NewLazyDLL("user32.dll").NewProc("GetWindowLongW")
	getClassNameProc              = syscall.NewLazyDLL("user32.dll").NewProc("GetClassNameW")
	getWindowThreadProcessIDProc  = syscall.NewLazyDLL("user32.dll").NewProc("GetWindowThreadProcessId")
)

// RunUIPreview opens the UI with sample values and without initializing hardware.
func (d *Deej) RunUIPreview() error {
	if err := d.config.Load(); err != nil {
		return fmt.Errorf("load config for UI preview: %w", err)
	}
	d.uiPreview = true
	d.openWindow()
	return nil
}

func (d *Deej) openWindow() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	d.windowLock.Lock()
	if d.windowOpen {
		d.windowLock.Unlock()
		return
	}
	d.windowOpen = true
	d.windowLock.Unlock()
	defer func() {
		d.windowLock.Lock()
		d.windowOpen = false
		d.windowLock.Unlock()
	}()

	window, err := walk.NewMainWindow()
	if err != nil {
		d.logger.Warnw("Failed to create deej UI", "error", err)
		return
	}
	var ui *deejUI
	defer func() {
		window.Dispose()
		if ui != nil {
			ui.disposeIcons()
		}
	}()
	if err := window.SetTitle("deej"); err != nil {
		d.logger.Warnw("Failed to set deej UI title", "error", err)
		return
	}
	if err := window.SetSize(walk.Size{Width: 1080, Height: 520}); err != nil {
		d.logger.Warnw("Failed to set deej UI size", "error", err)
	}
	rootLayout := walk.NewVBoxLayout()
	if err := rootLayout.SetAlignment(walk.AlignHNearVNear); err != nil {
		d.logger.Warnw("Failed to align deej UI layout", "error", err)
	}
	if err := window.SetLayout(rootLayout); err != nil {
		d.logger.Warnw("Failed to set deej UI layout", "error", err)
		return
	}

	ui = &deejUI{
		deej:         d,
		window:       window,
		stopRefresh:  make(chan struct{}),
		processPaths: make(map[string]string),
		icons:        make(map[string]*walk.Icon),
	}
	if err := ui.build(); err != nil {
		d.logger.Warnw("Failed to build deej UI", "error", err)
		return
	}

	window.Closing().Attach(func(_ *bool, _ walk.CloseReason) {
		atomic.StoreInt32(&ui.closed, 1)
		ui.stopOnce.Do(func() { close(ui.stopRefresh) })
	})
	go ui.refreshLoop()

	window.Show()
	window.Run()
	ui.stopOnce.Do(func() { close(ui.stopRefresh) })
	atomic.StoreInt32(&ui.closed, 1)
}

func (ui *deejUI) build() error {
	tabs, err := walk.NewTabWidget(ui.window)
	if err != nil {
		return err
	}
	livePage, err := walk.NewTabPage()
	if err != nil {
		return err
	}
	if err := livePage.SetTitle("Live mixer"); err != nil {
		return err
	}
	if err := livePage.SetLayout(walk.NewVBoxLayout()); err != nil {
		return err
	}
	settingsPage, err := walk.NewTabPage()
	if err != nil {
		return err
	}
	if err := settingsPage.SetTitle("Settings"); err != nil {
		return err
	}
	if err := settingsPage.SetLayout(walk.NewVBoxLayout()); err != nil {
		return err
	}
	if err := tabs.Pages().Add(livePage); err != nil {
		return err
	}
	if err := tabs.Pages().Add(settingsPage); err != nil {
		return err
	}

	statusRow, err := walk.NewComposite(ui.window)
	if err != nil {
		return err
	}
	if err := statusRow.SetMinMaxSize(walk.Size{Height: 34}, walk.Size{Height: 34}); err != nil {
		return err
	}
	if err := statusRow.SetLayout(walk.NewHBoxLayout()); err != nil {
		return err
	}
	ui.status, err = newUILabel(statusRow, "Waiting for Arduino input...")
	if err != nil {
		return err
	}
	refreshButton, err := newUIButton(statusRow, "Re-scan audio sessions")
	if err != nil {
		return err
	}
	refreshButton.SetEnabled(!ui.deej.uiPreview)
	refreshButton.Clicked().Attach(func() {
		ui.setStatus("Re-scanning audio sessions...")
		go ui.deej.sessions.refreshSessions(true)
	})

	if err := ui.window.Layout().(*walk.BoxLayout).SetStretchFactor(tabs, 1); err != nil {
		return err
	}

	if err := ui.buildLivePage(livePage); err != nil {
		return err
	}
	return ui.buildSettingsPage(settingsPage)
}

func (ui *deejUI) buildLivePage(page *walk.TabPage) error {
	config := ui.deej.config.snapshot()
	heading, err := newUILabel(page, "Live mixer")
	if err != nil {
		return err
	}
	if err := heading.SetMinMaxSize(walk.Size{Height: 20}, walk.Size{Height: 20}); err != nil {
		return err
	}
	heading.SetTextColor(walk.RGB(45, 50, 61))
	description, err := newUILabel(page, "Edit a mapping or adjust levels directly. Hardware button states are shown below each slider.")
	if err != nil {
		return err
	}
	if err := description.SetMinMaxSize(walk.Size{Height: 20}, walk.Size{Height: 20}); err != nil {
		return err
	}

	defaultName := "Default output (Preview)"
	outputs := []string{}
	if !ui.deej.uiPreview {
		var err error
		defaultName, outputs, err = activeWindowsOutputEndpoints()
		if err != nil {
			ui.deej.logger.Warnw("Failed to enumerate audio output endpoints for UI", "error", err)
		}
	}
	ui.defaultOutput = defaultName
	ui.outputChoices = outputs
	if alternate := strings.TrimSpace(config.AudioOutputs.Alternate); alternate != "" && !containsFold(outputs, alternate) {
		ui.outputChoices = append(ui.outputChoices, alternate)
		sort.Strings(ui.outputChoices)
	}

	scroll, err := walk.NewScrollView(page)
	if err != nil {
		return err
	}
	scroll.SetScrollbars(true, false)
	if err := scroll.SetLayout(walk.NewHBoxLayout()); err != nil {
		return err
	}
	ui.sliderParent = scroll
	if err := page.Layout().(*walk.BoxLayout).SetStretchFactor(scroll, 1); err != nil {
		return err
	}

	sliders, _, _ := ui.deej.uiStateSnapshot()
	sliderCount, visibleSliderCount := sliderCounts(len(sliders), config)
	if err := ui.ensureSliderCards(scroll, sliderCount); err != nil {
		return err
	}
	ui.setVisibleSliderCards(visibleSliderCount)
	if err := ui.addEndpointCard(scroll, "master", ui.defaultEndpointLabel(), false); err != nil {
		return err
	}
	alternate := strings.TrimSpace(config.AudioOutputs.Alternate)
	if err := ui.addEndpointCard(scroll, alternate, ui.alternateEndpointLabel(), true); err != nil {
		return err
	}
	ui.updateMappingLabels()
	return nil
}

func sliderCounts(valueCount int, config canonicalConfigSnapshot) (total, visible int) {
	total = valueCount
	config.SliderMapping.iterate(func(id int, _ []string) {
		if id+1 > total {
			total = id + 1
		}
	})
	if total == 0 {
		total = 5
	}

	visible = total
	alternate := strings.TrimSpace(config.AudioOutputs.Alternate)
	for visible > 0 {
		targets, ok := config.SliderMapping.get(visible - 1)
		if !ok || len(targets) != 1 {
			break
		}
		target := strings.TrimSpace(targets[0])
		if !strings.EqualFold(target, masterSessionName) && (alternate == "" || !strings.EqualFold(target, alternate)) {
			break
		}
		visible--
	}

	return total, visible
}

func (ui *deejUI) buildSettingsPage(page *walk.TabPage) error {
	var err error
	if _, err := newUILabel(page, "Connection and hardware behavior"); err != nil {
		return err
	}
	if ui.comPortEdit, err = ui.addSettingLine(page, "Serial port"); err != nil {
		return err
	}
	if ui.baudRateEdit, err = ui.addSettingLine(page, "Baud rate"); err != nil {
		return err
	}
	ui.invertCheck, err = walk.NewCheckBox(page)
	if err != nil {
		return err
	}
	if err := ui.invertCheck.SetText("Invert physical sliders"); err != nil {
		return err
	}

	if _, err := newUILabel(page, "Noise reduction"); err != nil {
		return err
	}
	ui.noiseCombo, err = walk.NewDropDownBox(page)
	if err != nil {
		return err
	}
	if err := ui.noiseCombo.SetModel([]string{"low", "default", "high"}); err != nil {
		return err
	}

	saveButton, err := newUIButton(page, "Save and apply")
	if err != nil {
		return err
	}
	saveButton.SetEnabled(!ui.deej.uiPreview)
	ui.comPortEdit.SetEnabled(!ui.deej.uiPreview)
	ui.baudRateEdit.SetEnabled(!ui.deej.uiPreview)
	ui.invertCheck.SetEnabled(!ui.deej.uiPreview)
	ui.noiseCombo.SetEnabled(!ui.deej.uiPreview)
	ui.settingsNote, err = newUILabel(page, "")
	if err != nil {
		return err
	}
	saveButton.Clicked().Attach(ui.saveSettings)
	ui.loadSettings()
	return nil
}

func (ui *deejUI) addSettingLine(parent walk.Container, title string) (*walk.LineEdit, error) {
	row, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	if err := row.SetLayout(walk.NewHBoxLayout()); err != nil {
		return nil, err
	}
	if _, err := newUILabel(row, title); err != nil {
		return nil, err
	}
	edit, err := walk.NewLineEdit(row)
	if err != nil {
		return nil, err
	}
	if err := row.Layout().(*walk.BoxLayout).SetStretchFactor(edit, 1); err != nil {
		return nil, err
	}
	return edit, nil
}

func (ui *deejUI) ensureSliderCards(parent walk.Container, count int) error {
	for len(ui.sliderCards) < count {
		id := len(ui.sliderCards)
		card, err := walk.NewGroupBox(parent)
		if err != nil {
			return err
		}
		if err := card.SetTitle(""); err != nil {
			return err
		}
		if err := card.SetMinMaxSize(walk.Size{Width: 104}, walk.Size{Width: 104}); err != nil {
			return err
		}
		if err := card.SetLayout(walk.NewVBoxLayout()); err != nil {
			return err
		}
		header, err := walk.NewComposite(card)
		if err != nil {
			return err
		}
		if err := header.SetLayout(walk.NewHBoxLayout()); err != nil {
			return err
		}
		headerLabel, err := newUILabel(header, fmt.Sprintf("Slider %d", id+1))
		if err != nil {
			return err
		}
		if err := headerLabel.SetMinMaxSize(walk.Size{Width: 52}, walk.Size{Width: 52}); err != nil {
			return err
		}
		editButton, err := newUIButton(header, "✎")
		if err != nil {
			return err
		}
		if err := editButton.SetMinMaxSize(walk.Size{Width: 28, Height: 28}, walk.Size{Width: 28, Height: 28}); err != nil {
			return err
		}
		if err := editButton.SetToolTipText("Edit mapping"); err != nil {
			return err
		}
		editButton.SetEnabled(!ui.deej.uiPreview)
		mappingRow, err := walk.NewComposite(card)
		if err != nil {
			return err
		}
		if err := mappingRow.SetLayout(walk.NewHBoxLayout()); err != nil {
			return err
		}
		icon, err := walk.NewImageView(mappingRow)
		if err != nil {
			return err
		}
		icon.SetMode(walk.ImageViewModeCenter)
		if err := icon.SetMinMaxSize(walk.Size{Width: 52, Height: 28}, walk.Size{Width: 52, Height: 28}); err != nil {
			return err
		}
		icon.SetVisible(false)
		mapping, err := newUILabel(mappingRow, ui.mappingText(id))
		if err != nil {
			return err
		}
		if err := mapping.SetTextAlignment(walk.AlignCenter); err != nil {
			return err
		}
		if err := mapping.SetEllipsisMode(walk.EllipsisEnd); err != nil {
			return err
		}
		if err := mapping.SetMinMaxSize(walk.Size{Width: 80}, walk.Size{Width: 80}); err != nil {
			return err
		}
		if err := mapping.SetToolTipText(ui.mappingText(id)); err != nil {
			return err
		}
		slider, err := walk.NewSliderWithOrientation(card, walk.Vertical)
		if err != nil {
			return err
		}
		if err := slider.SetMinMaxSize(walk.Size{Width: 36, Height: 120}, walk.Size{Width: 36}); err != nil {
			return err
		}
		slider.SetRange(0, 100)
		slider.SetValue(0)
		slider.SetTracking(false)
		slider.SetEnabled(false)
		if err := card.Layout().(*walk.BoxLayout).SetStretchFactor(slider, 5); err != nil {
			return err
		}
		percent, err := newUILabel(card, "---")
		if err != nil {
			return err
		}
		if err := percent.SetTextAlignment(walk.AlignCenter); err != nil {
			return err
		}
		button, err := newUILabel(card, "●")
		if err != nil {
			return err
		}
		indicatorFont, err := walk.NewFont("Segoe UI Symbol", 20, 0)
		if err != nil {
			return err
		}
		button.SetFont(indicatorFont)
		if err := button.SetMinMaxSize(walk.Size{Width: 36, Height: 36}, walk.Size{Width: 36, Height: 36}); err != nil {
			return err
		}
		if err := button.SetTextAlignment(walk.AlignCenter); err != nil {
			return err
		}
		button.SetTextColor(walk.RGB(145, 151, 162))

		currentCard := &sliderCard{
			container:        card,
			mapping:          mapping,
			icon:             icon,
			slider:           slider,
			percent:          percent,
			button:           button,
			buttonColorState: buttonColorUnknown,
		}
		ui.sliderCards = append(ui.sliderCards, currentCard)
		editButton.Clicked().Attach(func() { ui.editSliderMapping(id) })
	}
	return nil
}

func (ui *deejUI) setVisibleSliderCards(visibleCount int) {
	for i, card := range ui.sliderCards {
		card.container.SetVisible(i < visibleCount)
	}
}

func (ui *deejUI) addEndpointCard(parent walk.Container, key, label string, alternate bool) error {
	card, err := walk.NewGroupBox(parent)
	if err != nil {
		return err
	}
	title := "Default output"
	if alternate {
		title = "Alternate output"
	}
	if err := card.SetTitle(title); err != nil {
		return err
	}
	if err := card.SetMinMaxSize(walk.Size{Width: 112}, walk.Size{Width: 112}); err != nil {
		return err
	}
	if err := card.SetLayout(walk.NewVBoxLayout()); err != nil {
		return err
	}
	name, err := newUILabel(card, label)
	if err != nil {
		return err
	}
	if err := name.SetTextAlignment(walk.AlignCenter); err != nil {
		return err
	}
	if err := name.SetEllipsisMode(walk.EllipsisEnd); err != nil {
		return err
	}
	if err := name.SetMinMaxSize(walk.Size{Width: 94}, walk.Size{Width: 94}); err != nil {
		return err
	}
	if err := name.SetToolTipText(label); err != nil {
		return err
	}
	if alternate {
		choose, err := newUIButton(card, "Choose output")
		if err != nil {
			return err
		}
		choose.SetEnabled(!ui.deej.uiPreview)
		choose.Clicked().Attach(ui.chooseAlternateEndpoint)
	}
	slider, err := walk.NewSliderWithOrientation(card, walk.Vertical)
	if err != nil {
		return err
	}
	if err := slider.SetMinMaxSize(walk.Size{Width: 36, Height: 120}, walk.Size{Width: 36}); err != nil {
		return err
	}
	if err := card.Layout().(*walk.BoxLayout).SetStretchFactor(slider, 5); err != nil {
		return err
	}
	slider.SetRange(0, 100)
	slider.SetTracking(false)
	slider.SetEnabled(false)
	percent, err := newUILabel(card, "---")
	if err != nil {
		return err
	}
	if err := percent.SetTextAlignment(walk.AlignCenter); err != nil {
		return err
	}
	if err := percent.SetToolTipText("Pre-master modifier from the physical output slider"); err != nil {
		return err
	}
	sliderID := defaultOutputModifierSliderID
	if alternate {
		sliderID = alternateOutputModifierSliderID
	}
	endpoint := &endpointCard{key: key, sliderID: sliderID, name: name, slider: slider, percent: percent}
	ui.endpointCards = append(ui.endpointCards, endpoint)
	return nil
}

func (ui *deejUI) editSliderMapping(sliderID int) {
	dialog, err := walk.NewDialog(ui.window)
	if err != nil {
		ui.setStatus("Could not open mapping editor: " + err.Error())
		return
	}
	defer dialog.Dispose()
	if err := dialog.SetTitle(fmt.Sprintf("Edit slider %d mapping", sliderID+1)); err != nil {
		ui.setStatus(err.Error())
		return
	}
	if err := dialog.SetSize(walk.Size{Width: 420, Height: 190}); err != nil {
		ui.setStatus(err.Error())
	}
	if err := dialog.SetLayout(walk.NewVBoxLayout()); err != nil {
		ui.setStatus(err.Error())
		return
	}
	if _, err := newUILabel(dialog, "Choose a running app with audio, or browse to an executable."); err != nil {
		ui.setStatus(err.Error())
		return
	}
	if _, err := newUILabel(dialog, "Applying a choice replaces this slider's current target group."); err != nil {
		ui.setStatus(err.Error())
		return
	}
	choices, err := ui.mappingChoices()
	if err != nil {
		ui.setStatus("Could not enumerate all running apps: " + err.Error())
	}
	combo, err := walk.NewComboBox(dialog)
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	if err := combo.SetModel(choices); err != nil {
		ui.setStatus(err.Error())
		return
	}
	targets, _ := ui.deej.config.snapshot().SliderMapping.get(sliderID)
	if len(targets) == 1 {
		if err := combo.SetText(targets[0]); err != nil {
			ui.setStatus(err.Error())
			return
		}
	} else if len(targets) > 1 {
		if err := combo.SetText(strings.Join(targets, ", ")); err != nil {
			ui.setStatus(err.Error())
			return
		}
	}
	buttonRow, err := walk.NewComposite(dialog)
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	if err := buttonRow.SetLayout(walk.NewHBoxLayout()); err != nil {
		ui.setStatus(err.Error())
		return
	}
	browse, err := newUIButton(buttonRow, "Browse .exe...")
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	save, err := newUIButton(buttonRow, "Apply mapping")
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	cancel, err := newUIButton(buttonRow, "Cancel")
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	browse.Clicked().Attach(func() {
		fileDialog := &walk.FileDialog{
			Title:  "Choose an application",
			Filter: "Executables (*.exe)|*.exe|All files (*.*)|*.*",
		}
		accepted, err := fileDialog.ShowOpen(dialog)
		if err != nil {
			ui.setStatus("Could not open executable picker: " + err.Error())
			return
		}
		if accepted {
			if err := combo.SetText(filepath.Base(fileDialog.FilePath)); err != nil {
				ui.setStatus(err.Error())
			}
		}
	})
	save.Clicked().Attach(func() {
		target := strings.TrimSpace(strings.TrimSuffix(combo.Text(), " [audio]"))
		if target == "" {
			ui.setStatus("Choose an app before applying the mapping.")
			return
		}
		contents, err := ioutil.ReadFile(userConfigFilepath)
		if err != nil {
			ui.setStatus("Could not read config.yaml: " + err.Error())
			return
		}
		updated := setSliderMappingTarget(string(contents), sliderID, target)
		if err := ui.deej.config.ApplyUserConfig([]byte(updated)); err != nil {
			ui.setStatus("Could not apply mapping: " + err.Error())
			return
		}
		ui.updateMappingLabels()
		ui.setStatus(fmt.Sprintf("Slider %d now maps to %s.", sliderID+1, target))
		dialog.Accept()
	})
	cancel.Clicked().Attach(dialog.Cancel)
	dialog.Run()
}

func (ui *deejUI) chooseAlternateEndpoint() {
	dialog, err := walk.NewDialog(ui.window)
	if err != nil {
		ui.setStatus("Could not open endpoint picker: " + err.Error())
		return
	}
	defer dialog.Dispose()
	if err := dialog.SetTitle("Choose alternate audio output"); err != nil {
		ui.setStatus(err.Error())
		return
	}
	if err := dialog.SetSize(walk.Size{Width: 420, Height: 150}); err != nil {
		ui.setStatus(err.Error())
	}
	if err := dialog.SetLayout(walk.NewVBoxLayout()); err != nil {
		ui.setStatus(err.Error())
		return
	}
	if _, err := newUILabel(dialog, "Select an active output endpoint for pressed hardware buttons."); err != nil {
		ui.setStatus(err.Error())
		return
	}
	combo, err := walk.NewDropDownBox(dialog)
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	if err := combo.SetModel(ui.outputChoices); err != nil {
		ui.setStatus(err.Error())
		return
	}
	config := ui.deej.config.snapshot()
	for i, choice := range ui.outputChoices {
		if strings.EqualFold(choice, config.AudioOutputs.Alternate) {
			if err := combo.SetCurrentIndex(i); err != nil {
				ui.setStatus(err.Error())
			}
			break
		}
	}
	buttonRow, err := walk.NewComposite(dialog)
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	if err := buttonRow.SetLayout(walk.NewHBoxLayout()); err != nil {
		ui.setStatus(err.Error())
		return
	}
	save, err := newUIButton(buttonRow, "Apply endpoint")
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	cancel, err := newUIButton(buttonRow, "Cancel")
	if err != nil {
		ui.setStatus(err.Error())
		return
	}
	save.Clicked().Attach(func() {
		index := combo.CurrentIndex()
		if index < 0 || index >= len(ui.outputChoices) {
			ui.setStatus("Select an output endpoint.")
			return
		}
		contents, err := ioutil.ReadFile(userConfigFilepath)
		if err != nil {
			ui.setStatus("Could not read config.yaml: " + err.Error())
			return
		}
		updated := updateAudioAlternate(string(contents), ui.outputChoices[index])
		if err := ui.deej.config.ApplyUserConfig([]byte(updated)); err != nil {
			ui.setStatus("Could not apply endpoint: " + err.Error())
			return
		}
		ui.relabelEndpointCards()
		ui.setStatus("Alternate output set to " + ui.outputChoices[index] + ".")
		dialog.Accept()
	})
	cancel.Clicked().Attach(dialog.Cancel)
	dialog.Run()
}

func (ui *deejUI) saveSettings() {
	baudRate, err := strconv.Atoi(strings.TrimSpace(ui.baudRateEdit.Text()))
	if err != nil || baudRate <= 0 {
		ui.setSettingsNote("Baud rate must be a positive number.")
		return
	}
	comPort := strings.TrimSpace(ui.comPortEdit.Text())
	if comPort == "" {
		ui.setSettingsNote("Serial port cannot be empty.")
		return
	}
	noise := strings.ToLower(strings.TrimSpace(ui.noiseCombo.Text()))
	if noise != "low" && noise != "default" && noise != "high" {
		ui.setSettingsNote("Choose low, default, or high noise reduction.")
		return
	}
	contents, err := ioutil.ReadFile(userConfigFilepath)
	if err != nil {
		ui.setSettingsNote("Could not read config.yaml: " + err.Error())
		return
	}
	updated := string(contents)
	updated = setTopLevelConfigValue(updated, configKeyCOMPort, strconv.Quote(comPort))
	updated = setTopLevelConfigValue(updated, configKeyBaudRate, strconv.Itoa(baudRate))
	updated = setTopLevelConfigValue(updated, configKeyInvertSliders, strconv.FormatBool(ui.invertCheck.Checked()))
	updated = setTopLevelConfigValue(updated, configKeyNoiseReductionLevel, strconv.Quote(noise))
	if err := ui.deej.config.ApplyUserConfig([]byte(updated)); err != nil {
		ui.setSettingsNote("Could not apply settings: " + err.Error())
		return
	}
	ui.setSettingsNote("Settings saved and applied. Serial reconnect may take a moment.")
	ui.setStatus("Settings saved and applied.")
}

func (ui *deejUI) loadSettings() {
	config := ui.deej.config.snapshot()
	if err := ui.comPortEdit.SetText(config.COMPort); err != nil {
		ui.setSettingsNote("Could not load serial port setting: " + err.Error())
		return
	}
	if err := ui.baudRateEdit.SetText(strconv.Itoa(config.BaudRate)); err != nil {
		ui.setSettingsNote("Could not load baud rate setting: " + err.Error())
		return
	}
	if config.InvertSliders {
		ui.invertCheck.SetCheckState(walk.CheckChecked)
	} else {
		ui.invertCheck.SetCheckState(walk.CheckUnchecked)
	}
	noise := strings.ToLower(config.NoiseReductionLevel)
	if noise == "" {
		noise = "default"
	}
	for i, choice := range []string{"low", "default", "high"} {
		if noise == choice {
			if err := ui.noiseCombo.SetCurrentIndex(i); err != nil {
				ui.setSettingsNote("Could not load noise reduction setting: " + err.Error())
			}
			break
		}
	}
}

func (ui *deejUI) mappingChoices() ([]string, error) {
	applications, err := runningApplicationProcesses()
	if err != nil {
		return nil, err
	}
	audioApplications := make(map[string]bool)
	for _, application := range ui.deej.sessions.availableApplications() {
		audioApplications[strings.ToLower(application)] = true
	}
	choices := make([]string, 0, len(applications)+5)
	for name := range applications {
		if audioApplications[name] {
			name += " [audio]"
		}
		choices = append(choices, name)
	}
	choices = append(choices, masterSessionName, inputSessionName, systemSessionName,
		specialTargetTransformPrefix+specialTargetAllUnmapped,
		specialTargetTransformPrefix+specialTargetCurrentWindow)
	sort.Strings(choices)
	return uniqueStrings(choices), nil
}

func runningApplicationProcesses() (map[string]uint32, error) {
	processes, err := ps.Processes()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	processNames := make(map[uint32]string, len(processes))
	for _, process := range processes {
		name := strings.ToLower(filepath.Base(process.Executable()))
		// Task Manager treats the text input host as a background process rather than an app.
		if strings.EqualFold(filepath.Ext(name), ".exe") && name != "textinputhost.exe" {
			processNames[uint32(process.Pid())] = name
		}
	}

	applications := make(map[string]uint32)
	callback := syscall.NewCallback(func(window, _ uintptr) uintptr {
		visible, _, _ := isWindowVisibleProc.Call(window)
		if visible == 0 {
			return 1
		}
		titleLength, _, _ := getWindowTextLengthProc.Call(window)
		if titleLength == 0 {
			return 1
		}
		owner, _, _ := getWindowProc.Call(window, windowOwner)
		exStyle, _, _ := getWindowLongProc.Call(window, windowExtendedStyleIndex)
		if exStyle&windowExStyleTool != 0 || (owner != 0 && exStyle&windowExStyleApp == 0) {
			return 1
		}
		classNameBuffer := make([]uint16, 256)
		classNameLength, _, _ := getClassNameProc.Call(
			window,
			uintptr(unsafe.Pointer(&classNameBuffer[0])),
			uintptr(len(classNameBuffer)),
		)
		className := strings.ToLower(syscall.UTF16ToString(classNameBuffer[:classNameLength]))
		switch className {
		case "progman", "workerw", "shell_traywnd", "shell_secondarytraywnd":
			return 1
		}
		var processID uint32
		getWindowThreadProcessIDProc.Call(window, uintptr(unsafe.Pointer(&processID)))
		if name, ok := processNames[processID]; ok {
			applications[name] = processID
		}
		return 1
	})
	result, _, callErr := enumWindowsProc.Call(callback, 0)
	if result == 0 {
		if callErr == syscall.Errno(0) {
			return nil, fmt.Errorf("enumerate top-level windows failed")
		}
		return nil, fmt.Errorf("enumerate top-level windows: %w", callErr)
	}
	return applications, nil
}

func (ui *deejUI) mappingText(sliderID int) string {
	targets, ok := ui.deej.config.snapshot().SliderMapping.get(sliderID)
	if !ok || len(targets) == 0 {
		return "Unassigned"
	}
	return strings.Join(targets, ", ")
}

func (ui *deejUI) updateMappingLabels() {
	config := ui.deej.config.snapshot()
	for sliderID, card := range ui.sliderCards {
		mapping := ui.mappingText(sliderID)
		if err := card.mapping.SetText(mapping); err != nil {
			ui.deej.logger.Warnw("Failed to refresh slider mapping label", "sliderID", sliderID, "error", err)
		}
		if err := card.mapping.SetToolTipText(mapping); err != nil {
			ui.deej.logger.Warnw("Failed to refresh slider mapping tooltip", "sliderID", sliderID, "error", err)
		}
	}
	ui.updateMappingIcons(config)
}

func (ui *deejUI) updateMappingIcons(config canonicalConfigSnapshot) {
	ui.loadRunningProcessPaths(config)
	for sliderID, card := range ui.sliderCards {
		targets, _ := config.SliderMapping.get(sliderID)
		icon := ui.iconForMapping(targets)
		if icon != nil {
			if err := card.icon.SetToolTipText(ui.mappingText(sliderID)); err != nil {
				ui.deej.logger.Warnw("Failed to set slider mapping icon tooltip", "sliderID", sliderID, "error", err)
			}
		}
		if card.displayedIcon == icon {
			continue
		}
		card.displayedIcon = icon
		var image walk.Image
		if icon != nil {
			image = icon
		}
		if err := card.icon.SetImage(image); err != nil {
			ui.deej.logger.Warnw("Failed to refresh slider mapping icon", "sliderID", sliderID, "error", err)
		}
		if icon == nil {
			card.icon.SetVisible(false)
			card.mapping.SetVisible(true)
		} else {
			card.icon.SetVisible(true)
			card.mapping.SetVisible(false)
		}
	}
}

func (ui *deejUI) loadRunningProcessPaths(config canonicalConfigSnapshot) {
	ui.processPaths = make(map[string]string)
	wanted := make(map[string]bool)
	config.SliderMapping.iterate(func(_ int, targets []string) {
		for _, target := range targets {
			name := strings.TrimSpace(target)
			if strings.EqualFold(filepath.Ext(name), ".exe") {
				wanted[strings.ToLower(filepath.Base(name))] = true
			}
		}
	})
	if len(wanted) == 0 {
		return
	}

	applications, err := runningApplicationProcesses()
	if err != nil {
		ui.deej.logger.Debugw("Could not enumerate processes for mapping icons", "error", err)
		return
	}
	for name, processID := range applications {
		if !wanted[name] || ui.processPaths[name] != "" {
			continue
		}
		path, err := processExecutablePath(int(processID))
		if err != nil {
			ui.deej.logger.Debugw("Could not resolve process path for mapping icon", "process", name, "error", err)
			continue
		}
		ui.processPaths[name] = path
	}
}

func processExecutablePath(pid int) (string, error) {
	process, err := winapi.OpenProcess(winapi.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer winapi.CloseHandle(process)

	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	result, _, callErr := queryFullProcessImageNameProc.Call(
		uintptr(process),
		0,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if result == 0 {
		if callErr == syscall.Errno(0) {
			return "", fmt.Errorf("query process %d image path failed", pid)
		}
		return "", fmt.Errorf("query process %d image path: %w", pid, callErr)
	}

	return syscall.UTF16ToString(buffer[:size]), nil
}

func (ui *deejUI) iconForMapping(targets []string) *walk.Icon {
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if !strings.EqualFold(filepath.Ext(target), ".exe") {
			continue
		}
		path := ui.processPaths[strings.ToLower(filepath.Base(target))]
		if path == "" {
			continue
		}

		key := strings.ToLower(path)
		if icon, ok := ui.icons[key]; ok {
			return icon
		}
		icon, err := walk.NewIconExtractedFromFileWithSize(path, 0, 20)
		if err != nil {
			ui.deej.logger.Debugw("No application icon available for mapping", "path", path, "error", err)
			continue
		}
		ui.icons[key] = icon
		return icon
	}
	return nil
}

func (ui *deejUI) disposeIcons() {
	for _, icon := range ui.icons {
		icon.Dispose()
	}
}

func (ui *deejUI) relabelEndpointCards() {
	if len(ui.endpointCards) > 0 {
		if err := ui.endpointCards[0].name.SetText(ui.defaultEndpointLabel()); err != nil {
			ui.deej.logger.Warnw("Failed to refresh default output label", "error", err)
		}
	}
	if len(ui.endpointCards) > 1 {
		alternate := strings.TrimSpace(ui.deej.config.snapshot().AudioOutputs.Alternate)
		ui.endpointCards[1].key = alternate
		if err := ui.endpointCards[1].name.SetText(ui.alternateEndpointLabel()); err != nil {
			ui.deej.logger.Warnw("Failed to refresh alternate output label", "error", err)
		}
		if err := ui.endpointCards[1].name.SetToolTipText(ui.alternateEndpointLabel()); err != nil {
			ui.deej.logger.Warnw("Failed to refresh alternate output tooltip", "error", err)
		}
	}
}

func (ui *deejUI) defaultEndpointLabel() string {
	if ui.defaultOutput != "" {
		return ui.defaultOutput
	}
	return "Default output"
}

func (ui *deejUI) alternateEndpointLabel() string {
	alternate := strings.TrimSpace(ui.deej.config.snapshot().AudioOutputs.Alternate)
	if alternate == "" {
		return "Choose alternate output"
	}
	return alternate
}

func (ui *deejUI) refreshLoop() {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ui.stopRefresh:
			return
		case <-ticker.C:
			if atomic.LoadInt32(&ui.closed) != 0 {
				return
			}
			ui.window.Synchronize(func() {
				if atomic.LoadInt32(&ui.closed) == 0 {
					ui.refresh()
				}
			})
		}
	}
}

func (ui *deejUI) refresh() {
	sliders, buttons, buttonKnown := ui.deej.uiStateSnapshot()
	config := ui.deej.config.snapshot()
	sliderCount, visibleSliderCount := sliderCounts(len(sliders), config)
	if sliderCount > len(ui.sliderCards) {
		if err := ui.ensureSliderCards(ui.sliderParent, sliderCount); err != nil {
			ui.deej.logger.Warnw("Failed to add slider controls", "error", err)
		}
	}
	ui.setVisibleSliderCards(visibleSliderCount)
	if time.Since(ui.lastIconRefresh) >= 2*time.Second {
		ui.updateMappingIcons(config)
	}
	if ui.deej.uiPreview {
		sliders = make([]float32, len(ui.sliderCards))
		buttons = make([]bool, len(ui.sliderCards))
		sampleValues := []float32{0.73, 0.54, 0.89, 0.40, 0.65}
		for i := range sliders {
			sliders[i] = sampleValues[i%len(sampleValues)]
			buttons[i] = i%4 != 1
		}
		buttonKnown = true
	}
	if ui.deej.uiPreview {
		ui.setStatus("Preview mode - sample data only")
	} else if len(sliders) == 0 {
		ui.setStatus("Waiting for Arduino input...")
	} else {
		ui.setStatus(fmt.Sprintf("Arduino connected - %d sliders", len(sliders)))
	}

	for i, card := range ui.sliderCards {
		if i >= len(sliders) || sliders[i] < 0 {
			ui.setUILabel(card.percent, "---")
			continue
		}
		value := int(sliders[i] * 100)
		card.slider.SetValue(100 - value)
		ui.setUILabel(card.percent, fmt.Sprintf("%d%%", value))
	}
	for i, card := range ui.sliderCards {
		state := buttonColorUnknown
		if buttonKnown && i < len(buttons) {
			if buttons[i] {
				state = buttonColorReleased
			} else {
				state = buttonColorPressed
			}
		}
		if state == card.buttonColorState {
			continue
		}

		color := walk.RGB(145, 151, 162)
		switch state {
		case buttonColorPressed:
			color = walk.RGB(217, 76, 76)
		case buttonColorReleased:
			color = walk.RGB(56, 183, 108)
		}
		card.button.SetTextColor(color)
		card.buttonColorState = state
	}
	ui.refreshEndpointModifiers(sliders)
}

func (ui *deejUI) refreshEndpointModifiers(sliders []float32) {
	for _, endpoint := range ui.endpointCards {
		var value float32
		if ui.deej.uiPreview {
			value = 0.78
			if endpoint.sliderID == alternateOutputModifierSliderID {
				value = 0.62
			}
		} else if endpoint.sliderID >= len(sliders) || sliders[endpoint.sliderID] < 0 {
			ui.setUILabel(endpoint.percent, "---")
			continue
		} else {
			value = sliders[endpoint.sliderID]
		}
		valueInt := int(value * 100)
		endpoint.slider.SetValue(100 - valueInt)
		ui.setUILabel(endpoint.percent, fmt.Sprintf("Modifier %d%%", valueInt))
	}
}

func (ui *deejUI) setStatus(text string) error {
	err := ui.status.SetText(text)
	if err != nil {
		ui.deej.logger.Warnw("Failed to update deej UI status", "error", err)
	}
	return err
}

func (ui *deejUI) setSettingsNote(text string) error {
	err := ui.settingsNote.SetText(text)
	if err != nil {
		ui.deej.logger.Warnw("Failed to update deej UI settings status", "error", err)
	}
	return err
}

func (ui *deejUI) setUILabel(label *walk.Label, text string) {
	if err := label.SetText(text); err != nil {
		ui.deej.logger.Warnw("Failed to update deej UI label", "error", err)
	}
}

func activeWindowsOutputEndpoints() (string, []string, error) {
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		const sFalse = 1
		var oleError *ole.OleError
		if !errors.As(err, &oleError) || oleError.Code() != sFalse {
			return "", nil, fmt.Errorf("initialize COM for endpoint list: %w", err)
		}
	}
	defer ole.CoUninitialize()

	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&enumerator,
	); err != nil {
		return "", nil, fmt.Errorf("create audio endpoint enumerator: %w", err)
	}
	defer enumerator.Release()

	var defaultEndpoint *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &defaultEndpoint); err != nil {
		return "", nil, fmt.Errorf("get default audio output: %w", err)
	}
	defaultName, err := windowsEndpointName(defaultEndpoint)
	defaultEndpoint.Release()
	if err != nil {
		return "", nil, err
	}

	var collection *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return "", nil, fmt.Errorf("enumerate active audio outputs: %w", err)
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return "", nil, fmt.Errorf("get audio output count: %w", err)
	}
	outputs := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		var endpoint *wca.IMMDevice
		if err := collection.Item(i, &endpoint); err != nil {
			return "", nil, fmt.Errorf("get audio output %d: %w", i, err)
		}
		name, nameErr := windowsEndpointName(endpoint)
		endpoint.Release()
		if nameErr != nil {
			return "", nil, nameErr
		}
		outputs = append(outputs, name)
	}
	sort.Strings(outputs)
	return defaultName, uniqueStrings(outputs), nil
}

func windowsEndpointName(endpoint *wca.IMMDevice) (string, error) {
	var properties *wca.IPropertyStore
	if err := endpoint.OpenPropertyStore(wca.STGM_READ, &properties); err != nil {
		return "", fmt.Errorf("open audio output properties: %w", err)
	}
	defer properties.Release()

	value := &wca.PROPVARIANT{}
	if err := properties.GetValue(&wca.PKEY_Device_FriendlyName, value); err != nil {
		return "", fmt.Errorf("read audio output name: %w", err)
	}
	return value.String(), nil
}

func newUILabel(parent walk.Container, text string) (*walk.Label, error) {
	label, err := walk.NewLabel(parent)
	if err != nil {
		return nil, err
	}
	if err := label.SetText(text); err != nil {
		return nil, err
	}
	return label, nil
}

func newUIButton(parent walk.Container, text string) (*walk.PushButton, error) {
	button, err := walk.NewPushButton(parent)
	if err != nil {
		return nil, err
	}
	if err := button.SetText(text); err != nil {
		return nil, err
	}
	return button, nil
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if !strings.EqualFold(value, result[len(result)-1]) {
			result = append(result, value)
		}
	}
	return result
}
