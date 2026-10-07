//go:build windows

package service

// 最小 Windows 服务控制管理器（SCM）接入，纯 syscall 实现，零第三方依赖。
// 使 vrndisd.exe service-run 可被 sc.exe 注册为 Windows 服务：
//   - StartServiceCtrlDispatcherW 接入 SCM 调度
//   - RegisterServiceCtrlHandlerExW 响应 STOP/SHUTDOWN
//   - SetServiceStatus 上报 RUNNING/STOPPED
//
// 该文件仅在 Windows 上编译；语法由 GOOS=windows go build 验证，
// 运行时验证需 Windows 主机（见 docs/robustness-plan.md 平台矩阵）。

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	advapi32                        = syscall.NewLazyDLL("advapi32.dll")
	procStartServiceCtrlDispatcherW = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus            = advapi32.NewProc("SetServiceStatus")
)

const (
	serviceWin32OwnProcess    = 0x00000010
	serviceStopped            = 0x00000001
	serviceStartPending       = 0x00000002
	serviceStopPending        = 0x00000003
	serviceRunning            = 0x00000004
	serviceControlStop        = 0x00000001
	serviceControlShutdown    = 0x00000005
	serviceAcceptStop         = 0x00000001
	serviceAcceptShutdown     = 0x00000004
	errorFailedServiceConnect = 1063
)

type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

type serviceTableEntry struct {
	Name *uint16
	Proc uintptr
}

func setStatus(handle uintptr, st *serviceStatus) {
	procSetServiceStatus.Call(handle, uintptr(unsafe.Pointer(st)))
}

// runWithSCM 接入 SCM 并运行 fn；收到 STOP/SHUTDOWN 时取消 ctx。
func runWithSCM(name string, fn func(ctx context.Context) error) error {
	namep, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var statusHandle uintptr // RegisterServiceCtrlHandlerExW 返回的句柄
	st := &serviceStatus{ServiceType: serviceWin32OwnProcess, CurrentState: serviceStartPending, WaitHint: 5000}

	handler := syscall.NewCallback(func(control, eventType, eventData, contextArg uintptr) uintptr {
		switch uint32(control) {
		case serviceControlStop, serviceControlShutdown:
			st.CurrentState = serviceStopPending
			st.WaitHint = 10000
			setStatus(statusHandle, st)
			cancel()
		}
		return 0
	})

	runErr := make(chan error, 1)
	svcMain := syscall.NewCallback(func(argc, argv uintptr) uintptr {
		handle, _, _ := procRegisterServiceCtrlHandlerW.Call(
			uintptr(unsafe.Pointer(namep)), handler, 0)
		if handle == 0 {
			runErr <- fmt.Errorf("RegisterServiceCtrlHandlerExW 失败")
			return 1
		}
		statusHandle = handle
		st.CurrentState = serviceRunning
		st.ControlsAccepted = serviceAcceptStop | serviceAcceptShutdown
		st.Win32ExitCode = 0
		setStatus(statusHandle, st)

		err := fn(ctx)

		st.CurrentState = serviceStopped
		if err != nil {
			st.Win32ExitCode = 1 // ERROR_SERVICE_SPECIFIC_ERROR 用 1 简化
		}
		setStatus(statusHandle, st)
		runErr <- err
		return 0
	})

	table := [2]serviceTableEntry{{Name: namep, Proc: svcMain}, {}}
	ret, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if ret == 0 {
		// 1063：不是被 SCM 启动的（交互式运行）——退化为前台运行
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorFailedServiceConnect {
			return fn(ctx)
		}
		return fmt.Errorf("StartServiceCtrlDispatcherW 失败: %v", callErr)
	}
	return <-runErr
}
