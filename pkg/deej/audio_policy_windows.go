package deej

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	wca "github.com/moutend/go-wca"
)

var (
	combaseDLL                 = syscall.NewLazyDLL("combase.dll")
	roGetActivationFactoryProc = combaseDLL.NewProc("RoGetActivationFactory")
	windowsCreateStringProc    = combaseDLL.NewProc("WindowsCreateString")
	windowsDeleteStringProc    = combaseDLL.NewProc("WindowsDeleteString")
)

const (
	audioPolicyConfigClass = "Windows.Media.Internal.AudioPolicyConfig"
)

type audioPolicyConfigFactory struct {
	vtable *audioPolicyConfigFactoryVTable
}

type audioPolicyConfigFactoryVTable struct {
	queryInterface                   uintptr
	addRef                           uintptr
	release                          uintptr
	getIids                          uintptr
	getRuntimeName                   uintptr
	getTrustLevel                    uintptr
	priorMethods                     [19]uintptr
	setPersistedDefaultAudioEndpoint uintptr
}

type unknownCOMObject struct {
	vtable *unknownCOMObjectVTable
}

type unknownCOMObjectVTable struct {
	queryInterface uintptr
	addRef         uintptr
	release        uintptr
}

func setWindowsProcessOutput(processID uint32, endpointName string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		const sFalse = 1
		var oleError *ole.OleError
		if !errors.As(err, &oleError) || oleError.Code() != sFalse {
			return fmt.Errorf("initialize COM: %w", err)
		}
	}
	defer ole.CoUninitialize()

	endpointID := ""
	if endpointName != "" {
		resolvedID, err := findRenderEndpointID(endpointName)
		if err != nil {
			return err
		}
		endpointID = resolvedID
	}

	return setPersistedProcessEndpoint(processID, endpointID)
}

func findRenderEndpointID(endpointName string) (string, error) {
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&enumerator,
	); err != nil {
		return "", fmt.Errorf("create audio device enumerator: %w", err)
	}
	defer enumerator.Release()

	var devices *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &devices); err != nil {
		return "", fmt.Errorf("enumerate output endpoints: %w", err)
	}
	defer devices.Release()

	var count uint32
	if err := devices.GetCount(&count); err != nil {
		return "", fmt.Errorf("get output endpoint count: %w", err)
	}

	for index := uint32(0); index < count; index++ {
		var endpoint *wca.IMMDevice
		if err := devices.Item(index, &endpoint); err != nil {
			return "", fmt.Errorf("get output endpoint %d: %w", index, err)
		}

		var properties *wca.IPropertyStore
		if err := endpoint.OpenPropertyStore(wca.STGM_READ, &properties); err != nil {
			endpoint.Release()
			return "", fmt.Errorf("open output endpoint properties: %w", err)
		}

		value := &wca.PROPVARIANT{}
		err := properties.GetValue(&wca.PKEY_Device_FriendlyName, value)
		friendlyName := value.String()
		properties.Release()

		if err != nil {
			endpoint.Release()
			return "", fmt.Errorf("read output endpoint name: %w", err)
		}

		if strings.EqualFold(friendlyName, endpointName) {
			var endpointID string
			err = endpoint.GetId(&endpointID)
			endpoint.Release()
			if err != nil {
				return "", fmt.Errorf("get endpoint ID for %q: %w", endpointName, err)
			}
			return endpointID, nil
		}
		endpoint.Release()
	}

	return "", fmt.Errorf("active output endpoint %q was not found", endpointName)
}

func setPersistedProcessEndpoint(processID uint32, endpointID string) error {
	deviceString := uintptr(0)
	if endpointID != "" {
		devicePath := fmt.Sprintf(`\\?\SWD#MMDEVAPI#%s#{e6327cad-dcec-4949-ae8a-991e976a79d2}`, endpointID)
		var err error
		deviceString, err = createHString(devicePath)
		if err != nil {
			return fmt.Errorf("create endpoint HSTRING: %w", err)
		}
		defer windowsDeleteStringProc.Call(deviceString)
	}

	factory, err := createAudioPolicyFactory()
	if err != nil {
		return err
	}
	defer releaseCOMObject(factory)

	methodAddress := (*audioPolicyConfigFactory)(factory).vtable.setPersistedDefaultAudioEndpoint
	for _, role := range []uint32{wca.EConsole, wca.EMultimedia} {
		hresult, _, _ := syscall.Syscall6(
			methodAddress,
			5,
			uintptr(factory),
			uintptr(processID),
			uintptr(wca.ERender),
			uintptr(role),
			deviceString,
			0,
		)
		if int32(hresult) < 0 {
			return fmt.Errorf("set persisted output for process %d (role %d): HRESULT 0x%08X", processID, role, uint32(hresult))
		}
	}

	return nil
}

func createAudioPolicyFactory() (unsafe.Pointer, error) {
	className, err := createHString(audioPolicyConfigClass)
	if err != nil {
		return nil, fmt.Errorf("create audio policy class HSTRING: %w", err)
	}
	defer windowsDeleteStringProc.Call(className)

	interfaceIDs := []string{
		"{ab3d4648-e242-459f-b02f-541c70306324}",
		"{2a59116d-6c4f-45e0-a74f-707e3fef9258}",
	}
	var lastHRESULT uintptr
	for _, interfaceID := range interfaceIDs {
		iid := ole.NewGUID(interfaceID)
		var factory unsafe.Pointer
		hresult, _, _ := roGetActivationFactoryProc.Call(
			className,
			uintptr(unsafe.Pointer(iid)),
			uintptr(unsafe.Pointer(&factory)),
		)
		if int32(hresult) >= 0 && factory != nil {
			return factory, nil
		}
		lastHRESULT = hresult
		if factory != nil {
			releaseCOMObject(factory)
		}
	}

	return nil, fmt.Errorf("activate audio policy config factory: HRESULT 0x%08X", uint32(lastHRESULT))
}

func createHString(value string) (uintptr, error) {
	encoded := utf16.Encode([]rune(value))
	var result uintptr
	hresult, _, _ := windowsCreateStringProc.Call(
		uintptr(unsafe.Pointer(&encoded[0])),
		uintptr(len(encoded)),
		uintptr(unsafe.Pointer(&result)),
	)
	if int32(hresult) < 0 {
		return 0, fmt.Errorf("WindowsCreateString returned HRESULT 0x%08X", uint32(hresult))
	}
	return result, nil
}

func releaseCOMObject(object unsafe.Pointer) {
	if object == nil {
		return
	}
	release := (*unknownCOMObject)(object).vtable.release
	syscall.Syscall6(release, 1, uintptr(object), 0, 0, 0, 0, 0)
}
