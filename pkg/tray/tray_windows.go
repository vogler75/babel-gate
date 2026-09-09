//go:build windows

package tray

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassEx        = user32.NewProc("RegisterClassExW")
	procCreateWindowEx         = user32.NewProc("CreateWindowExW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procDefWindowProc          = user32.NewProc("DefWindowProcW")
	procGetMessage             = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessage        = user32.NewProc("DispatchMessageW")
	procPostMessage            = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessage  = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procAppendMenu             = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procLoadIcon               = user32.NewProc("LoadIconW")
	procLoadCursor             = user32.NewProc("LoadCursorW")
	procDestroyIcon            = user32.NewProc("DestroyIcon")
	procCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procShellExecute    = shell32.NewProc("ShellExecuteW")

	procGetModuleHandle       = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
)

const (
	className  = "BabelGateTrayWindow"
	windowName = "BabelGate"

	wsOverlappedWindow = 0x00CF0000

	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmApp         = 0x8000
	wmTrayMessage = wmApp + 1 // notification-area callback
	wmTrayQuit    = wmApp + 2 // posted when ctx is cancelled

	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B

	nimAdd    = 0x0000
	nimDelete = 0x0002

	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004

	mfString    = 0x0000
	mfSeparator = 0x0800

	tpmLeftAlign = 0x0000
	tpmRightBtn  = 0x0002
	tpmReturnCmd = 0x0100

	idiApplication = 32512
	idcArrow       = 32512

	smCXSmIcon = 49
	smCYSmIcon = 50

	swShowNormal = 1

	lrDefaultColor = 0x0000

	errClassAlreadyExists = 1410

	// Menu command identifiers.
	cmdDashboard = 1
	cmdSetup     = 2
	cmdQuit      = 3
)

type point struct {
	x, y int32
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type msgStruct struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             syscall.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            syscall.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     syscall.Handle
}

// trayState is the single live tray instance. Only one may exist at a time
// because the window procedure is a package-level callback.
type trayState struct {
	opts        Options
	hwnd        syscall.Handle
	hicon       syscall.Handle
	nid         notifyIconData
	taskbarMsg  uint32
	ownedIcon   bool
	quitInvoked bool
}

var (
	active   atomic.Bool
	instance *trayState

	// Created once: Windows allows only a limited number of callbacks per
	// process, and wndProc is stateless (it reads the current instance).
	wndProcCallback = syscall.NewCallback(wndProc)
)

func supported() bool { return true }

func run(ctx context.Context, opts Options) error {
	if !active.CompareAndSwap(false, true) {
		return errors.New("tray: already running")
	}
	defer active.Store(false)

	// The window and its message loop must stay on one OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Safety net in case the caller did not already do this.
	hideConsole()

	st := &trayState{opts: opts}
	instance = st
	defer func() { instance = nil }()

	hInstance, _, _ := procGetModuleHandle.Call(0)

	if err := registerClass(syscall.Handle(hInstance)); err != nil {
		return err
	}

	classNamePtr, err := syscall.UTF16PtrFromString(className)
	if err != nil {
		return err
	}
	windowNamePtr, err := syscall.UTF16PtrFromString(windowName)
	if err != nil {
		return err
	}

	// A normal (never shown) top-level window rather than a message-only one:
	// message-only windows do not receive the broadcast "TaskbarCreated".
	hwnd, _, callErr := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(windowNamePtr)),
		wsOverlappedWindow,
		0, 0, 0, 0,
		0, 0, hInstance, 0,
	)
	runtime.KeepAlive(classNamePtr)
	runtime.KeepAlive(windowNamePtr)
	if hwnd == 0 {
		return fmt.Errorf("tray: CreateWindowEx failed: %w", callErr)
	}
	st.hwnd = syscall.Handle(hwnd)
	defer procDestroyWindow.Call(hwnd)

	// Re-add the icon if Explorer restarts.
	taskbarName := utf16Ptr("TaskbarCreated")
	m, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(taskbarName)))
	runtime.KeepAlive(taskbarName)
	if m != 0 {
		st.taskbarMsg = uint32(m)
	}

	st.hicon, st.ownedIcon = loadTrayIcon()
	if st.ownedIcon {
		defer procDestroyIcon.Call(uintptr(st.hicon))
	}

	st.nid = notifyIconData{
		hWnd:             st.hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayMessage,
		hIcon:            st.hicon,
	}
	st.nid.cbSize = uint32(unsafe.Sizeof(st.nid))
	setTip(&st.nid, opts.Tooltip)

	if err := st.notify(nimAdd); err != nil {
		return err
	}
	defer st.notify(nimDelete)

	// Translate context cancellation into a message-loop wakeup.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			procPostMessage.Call(hwnd, wmTrayQuit, 0, 0)
		case <-done:
		}
	}()

	messageLoop()
	return nil
}

func registerClass(hInstance syscall.Handle) error {
	hCursor, _, _ := procLoadCursor.Call(0, idcArrow)
	classNamePtr := utf16Ptr(className)
	wc := wndClassEx{
		lpfnWndProc:   wndProcCallback,
		hInstance:     hInstance,
		hCursor:       syscall.Handle(hCursor),
		lpszClassName: classNamePtr,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))

	atom, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	runtime.KeepAlive(&wc)
	runtime.KeepAlive(classNamePtr)
	if atom == 0 {
		// Re-registering the same class in one process is harmless.
		if errno, ok := callErr.(syscall.Errno); ok && errno == errClassAlreadyExists {
			return nil
		}
		return fmt.Errorf("tray: RegisterClassEx failed: %w", callErr)
	}
	return nil
}

func messageLoop() {
	var m msgStruct
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(ret) {
		case -1: // error
			return
		case 0: // WM_QUIT
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	st := instance
	if st == nil {
		ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
		return ret
	}

	switch message {
	case wmTrayMessage:
		switch uint32(lParam) {
		case wmLButtonUp, wmRButtonUp, wmContextMenu:
			st.showMenu()
		case wmLButtonDbl:
			openURL(st.opts.DashboardURL)
		}
		return 0

	case wmCommand:
		switch uint32(wParam & 0xFFFF) {
		case cmdDashboard:
			openURL(st.opts.DashboardURL)
		case cmdSetup:
			openURL(st.opts.SetupURL)
		case cmdQuit:
			st.quit()
		}
		return 0

	case wmTrayQuit:
		// Context cancelled elsewhere: tear down without re-firing OnQuit.
		st.quitInvoked = true
		procPostQuitMessage.Call(0)
		return 0

	case wmClose:
		st.quit()
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0

	default:
		if st.taskbarMsg != 0 && message == st.taskbarMsg {
			st.notify(nimAdd)
			return 0
		}
	}

	ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

func (st *trayState) quit() {
	if !st.quitInvoked {
		st.quitInvoked = true
		if st.opts.OnQuit != nil {
			st.opts.OnQuit()
		}
	}
	procPostQuitMessage.Call(0)
}

func (st *trayState) notify(action uint32) error {
	ret, _, callErr := procShellNotifyIcon.Call(uintptr(action), uintptr(unsafe.Pointer(&st.nid)))
	runtime.KeepAlive(st)
	if ret == 0 && action != nimDelete {
		return fmt.Errorf("tray: Shell_NotifyIcon(%d) failed: %w", action, callErr)
	}
	return nil
}

func (st *trayState) showMenu() {
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hmenu)

	appendMenuItem(hmenu, cmdDashboard, "Open Dashboard")
	if st.opts.SetupURL != "" {
		appendMenuItem(hmenu, cmdSetup, "Open Setup")
	}
	procAppendMenu.Call(hmenu, mfSeparator, 0, 0)
	appendMenuItem(hmenu, cmdQuit, "Quit BabelGate")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	runtime.KeepAlive(&pt)

	// Required so the menu dismisses when the user clicks elsewhere.
	procSetForegroundWindow.Call(uintptr(st.hwnd))

	cmd, _, _ := procTrackPopupMenu.Call(
		hmenu,
		tpmLeftAlign|tpmRightBtn|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y),
		0,
		uintptr(st.hwnd),
		0,
	)

	// Classic Win32 workaround so the menu closes cleanly.
	procPostMessage.Call(uintptr(st.hwnd), 0 /* WM_NULL */, 0, 0)

	if cmd != 0 {
		procPostMessage.Call(uintptr(st.hwnd), wmCommand, cmd, 0)
	}
}

func appendMenuItem(hmenu uintptr, id uint32, text string) {
	textPtr := utf16Ptr(text)
	procAppendMenu.Call(hmenu, mfString, uintptr(id), uintptr(unsafe.Pointer(textPtr)))
	runtime.KeepAlive(textPtr)
}

func openURL(url string) {
	if url == "" {
		return
	}
	verbPtr := utf16Ptr("open")
	urlPtr := utf16Ptr(url)
	procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(urlPtr)),
		0,
		0,
		swShowNormal,
	)
	runtime.KeepAlive(verbPtr)
	runtime.KeepAlive(urlPtr)
}

// hideConsole detaches from the console when this process is its only client,
// which is the case when launched from Explorer or a shortcut. Safe to call
// more than once: GetConsoleWindow returns 0 once detached.
func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	var pids [2]uint32
	count, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	runtime.KeepAlive(&pids)
	if count == 1 {
		procFreeConsole.Call()
	}
}

// loadTrayIcon returns the embedded icon, falling back to the stock application
// icon. The bool reports whether the handle must be destroyed by the caller.
func loadTrayIcon() (syscall.Handle, bool) {
	cx, _, _ := procGetSystemMetrics.Call(smCXSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCYSmIcon)
	if cx == 0 || cy == 0 {
		cx, cy = 16, 16
	}

	if h, ok := iconFromICO(iconData, int(cx), int(cy)); ok {
		return h, true
	}

	h, _, _ := procLoadIcon.Call(0, idiApplication)
	return syscall.Handle(h), false
}

// iconFromICO picks the ICONDIRENTRY closest to the requested size and hands
// its DIB to CreateIconFromResourceEx.
func iconFromICO(data []byte, cx, cy int) (syscall.Handle, bool) {
	offset, size, ok := pickIconEntry(data, cx)
	if !ok {
		return 0, false
	}

	h, _, _ := procCreateIconFromResource.Call(
		uintptr(unsafe.Pointer(&data[offset])),
		uintptr(size),
		1, // fIcon
		0x00030000,
		uintptr(cx),
		uintptr(cy),
		lrDefaultColor,
	)
	runtime.KeepAlive(data)
	if h == 0 {
		return 0, false
	}
	return syscall.Handle(h), true
}

func setTip(nid *notifyIconData, tip string) {
	if tip == "" {
		return
	}
	encoded, err := syscall.UTF16FromString(tip)
	if err != nil {
		return
	}
	if len(encoded) > len(nid.szTip) {
		encoded = encoded[:len(nid.szTip)-1]
		encoded = append(encoded, 0)
	}
	copy(nid.szTip[:], encoded)
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}
