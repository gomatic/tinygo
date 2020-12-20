package main

import (
	"machine"
	"time"
)

const (
	led    = machine.LED
	button = machine.BUTTON
)

func main() {
	led.Configure(machine.PinConfigOutput())
	button.Configure(machine.PinConfigInput())

	for {
		if button.Get() {
			led.Low()
		} else {
			led.High()
		}

		time.Sleep(time.Millisecond * 10)
	}
}
