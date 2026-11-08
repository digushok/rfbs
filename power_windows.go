//go:build windows

package main

import (
	"runtime"
	"syscall"
)

// Не даём компьютеру уходить в сон, пока работает автоматизация:
// во сне программа не может вовремя перевести заказы в «Условно доставлен».

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var awakeCh = make(chan bool, 1)

func init() {
	setState := syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")
	go func() {
		// Состояние привязано к потоку, поэтому держим один поток ОС.
		runtime.LockOSThread()
		for on := range awakeCh {
			if on {
				setState.Call(uintptr(esContinuous | esSystemRequired))
			} else {
				setState.Call(uintptr(esContinuous))
			}
		}
	}()
}

func keepAwake(on bool) {
	select {
	case <-awakeCh:
	default:
	}
	awakeCh <- on
}
