// +build nrf

package machine

import (
	"device/nrf"
	"errors"
	"runtime/interrupt"
)

var (
	ErrTxInvalidSliceSize = errors.New("SPI write and read slices must be same size")
)

type PinMode uint8

const (
	PinInput         PinMode = (nrf.GPIO_PIN_CNF_DIR_Input << nrf.GPIO_PIN_CNF_DIR_Pos) | (nrf.GPIO_PIN_CNF_INPUT_Connect << nrf.GPIO_PIN_CNF_INPUT_Pos)
	PinInputPullup   PinMode = PinInput | (nrf.GPIO_PIN_CNF_PULL_Pullup << nrf.GPIO_PIN_CNF_PULL_Pos)
	PinInputPulldown PinMode = PinInput | (nrf.GPIO_PIN_CNF_PULL_Pulldown << nrf.GPIO_PIN_CNF_PULL_Pos)
	PinOutput        PinMode = (nrf.GPIO_PIN_CNF_DIR_Output << nrf.GPIO_PIN_CNF_DIR_Pos) | (nrf.GPIO_PIN_CNF_INPUT_Disconnect << nrf.GPIO_PIN_CNF_INPUT_Pos)
)

// PinConfigOutput is a convenience function that returns a PinConfig struct for output mode.
func PinConfigOutput() PinConfig {
	return PinConfig{Mode: PinOutput}
}

// PinConfigInput is a convenience function that returns a PinConfig struct for input mode.
func PinConfigInput() PinConfig {
	return PinConfig{Mode: PinInputPullup}
}

type PinChange uint8

// Pin change interrupt constants for SetInterrupt.
const (
	PinRising  PinChange = nrf.GPIOTE_CONFIG_POLARITY_LoToHi
	PinFalling PinChange = nrf.GPIOTE_CONFIG_POLARITY_HiToLo
	PinToggle  PinChange = nrf.GPIOTE_CONFIG_POLARITY_Toggle
)

// Callbacks to be called for pins configured with SetInterrupt.
var pinCallbacks [len(nrf.GPIOTE.CONFIG)]func(Pin)

// Configure this pin with the given configuration.
func (p Pin) Configure(config PinConfig) {
	cfg := config.Mode | nrf.GPIO_PIN_CNF_DRIVE_S0S1 | nrf.GPIO_PIN_CNF_SENSE_Disabled
	port, pin := p.getPortPin()
	port.PIN_CNF[pin].Set(uint32(cfg))
}

// Set the pin to high or low.
// Warning: only use this on an output pin!
func (p Pin) Set(high bool) {
	port, pin := p.getPortPin()
	if high {
		port.OUTSET.Set(1 << pin)
	} else {
		port.OUTCLR.Set(1 << pin)
	}
}

// Return the register and mask to enable a given GPIO pin. This can be used to
// implement bit-banged drivers.
func (p Pin) PortMaskSet() (*uint32, uint32) {
	port, pin := p.getPortPin()
	return &port.OUTSET.Reg, 1 << pin
}

// Return the register and mask to disable a given port. This can be used to
// implement bit-banged drivers.
func (p Pin) PortMaskClear() (*uint32, uint32) {
	port, pin := p.getPortPin()
	return &port.OUTCLR.Reg, 1 << pin
}

// Get returns the current value of a GPIO pin.
func (p Pin) Get() bool {
	port, pin := p.getPortPin()
	return (port.IN.Get()>>pin)&1 != 0
}

// SetInterrupt sets an interrupt to be executed when a particular pin changes
// state. The pin should already be configured as an input, including a pull up
// or down if no external pull is provided.
//
// This call will replace a previously set callback on this pin. You can pass a
// nil func to unset the pin change interrupt. If you do so, the change
// parameter is ignored and can be set to any value (such as 0).
func (p Pin) SetInterrupt(change PinChange, callback func(Pin)) error {
	// Some variables to easily check whether a channel was already configured
	// as an event channel for the given pin.
	// This is not just an optimization, this is requred: the datasheet says
	// that configuring more than one channel for a given pin results in
	// unpredictable behavior.
	expectedConfigMask := uint32(nrf.GPIOTE_CONFIG_MODE_Msk | nrf.GPIOTE_CONFIG_PSEL_Msk)
	expectedConfig := nrf.GPIOTE_CONFIG_MODE_Event<<nrf.GPIOTE_CONFIG_MODE_Pos | uint32(p)<<nrf.GPIOTE_CONFIG_PSEL_Pos

	foundChannel := false
	for i := range nrf.GPIOTE.CONFIG {
		config := nrf.GPIOTE.CONFIG[i].Get()
		if config == 0 || config&expectedConfigMask == expectedConfig {
			// Found an empty GPIOTE channel or one that was already configured
			// for this pin.
			if callback == nil {
				// Disable this channel.
				nrf.GPIOTE.INTENCLR.Set(uint32(1 << uint(i)))
				pinCallbacks[i] = nil
				return nil
			}
			// Enable this channel with the given callback.
			nrf.GPIOTE.INTENCLR.Set(uint32(1 << uint(i)))
			nrf.GPIOTE.CONFIG[i].Set(nrf.GPIOTE_CONFIG_MODE_Event<<nrf.GPIOTE_CONFIG_MODE_Pos |
				uint32(p)<<nrf.GPIOTE_CONFIG_PSEL_Pos |
				uint32(change)<<nrf.GPIOTE_CONFIG_POLARITY_Pos)
			pinCallbacks[i] = callback
			nrf.GPIOTE.INTENSET.Set(uint32(1 << uint(i)))
			foundChannel = true
			break
		}
	}

	if !foundChannel {
		return ErrNoPinChangeChannel
	}

	// Set and enable the GPIOTE interrupt. It's not a problem if this happens
	// more than once.
	interrupt.New(nrf.IRQ_GPIOTE, func(interrupt.Interrupt) {
		for i := range nrf.GPIOTE.EVENTS_IN {
			if nrf.GPIOTE.EVENTS_IN[i].Get() != 0 {
				nrf.GPIOTE.EVENTS_IN[i].Set(0)
				pin := Pin((nrf.GPIOTE.CONFIG[i].Get() & nrf.GPIOTE_CONFIG_PSEL_Msk) >> nrf.GPIOTE_CONFIG_PSEL_Pos)
				pinCallbacks[i](pin)
			}
		}
	}).Enable()

	// Everything was configured correctly.
	return nil
}

// UART on the NRF.
type UART struct {
	Buffer *RingBuffer
}

// UART
var (
	// NRF_UART0 is the hardware UART on the NRF SoC.
	NRF_UART0 = UART{Buffer: NewRingBuffer()}
)

// Configure the UART.
func (uart UART) Configure(config UARTConfig) {
	// Default baud rate to 115200.
	if config.BaudRate == 0 {
		config.BaudRate = 115200
	}

	uart.SetBaudRate(config.BaudRate)

	// Set TX and RX pins
	if config.TX == 0 && config.RX == 0 {
		// Use default pins
		uart.setPins(UART_TX_PIN, UART_RX_PIN)
	} else {
		uart.setPins(config.TX, config.RX)
	}

	nrf.UART0.ENABLE.Set(nrf.UART_ENABLE_ENABLE_Enabled)
	nrf.UART0.TASKS_STARTTX.Set(1)
	nrf.UART0.TASKS_STARTRX.Set(1)
	nrf.UART0.INTENSET.Set(nrf.UART_INTENSET_RXDRDY_Msk)

	// Enable RX IRQ.
	intr := interrupt.New(nrf.IRQ_UART0, NRF_UART0.handleInterrupt)
	intr.SetPriority(0xc0) // low priority
	intr.Enable()
}

// SetBaudRate sets the communication speed for the UART.
func (uart UART) SetBaudRate(br uint32) {
	// Magic: calculate 'baudrate' register from the input number.
	// Every value listed in the datasheet will be converted to the
	// correct register value, except for 192600. I suspect the value
	// listed in the nrf52 datasheet (0x0EBED000) is incorrectly rounded
	// and should be 0x0EBEE000, as the nrf51 datasheet lists the
	// nonrounded value 0x0EBEDFA4.
	// Some background:
	// https://devzone.nordicsemi.com/f/nordic-q-a/391/uart-baudrate-register-values/2046#2046
	rate := uint32((uint64(br/400)*uint64(400*0xffffffff/16000000) + 0x800) & 0xffffff000)

	nrf.UART0.BAUDRATE.Set(rate)
}

// WriteByte writes a byte of data to the UART.
func (uart UART) WriteByte(c byte) error {
	nrf.UART0.EVENTS_TXDRDY.Set(0)
	nrf.UART0.TXD.Set(uint32(c))
	for nrf.UART0.EVENTS_TXDRDY.Get() == 0 {
	}
	return nil
}

func (uart *UART) handleInterrupt(interrupt.Interrupt) {
	if nrf.UART0.EVENTS_RXDRDY.Get() != 0 {
		uart.Receive(byte(nrf.UART0.RXD.Get()))
		nrf.UART0.EVENTS_RXDRDY.Set(0x0)
	}
}

// I2C on the NRF.
type I2C struct {
	Bus *nrf.TWI_Type
}

// There are 2 I2C interfaces on the NRF.
var (
	I2C0 = I2C{Bus: nrf.TWI0}
	I2C1 = I2C{Bus: nrf.TWI1}
)

// I2CConfig is used to store config info for I2C.
type I2CConfig struct {
	Frequency uint32
	SCL       Pin
	SDA       Pin
}

// Configure is intended to setup the I2C interface.
func (i2c I2C) Configure(config I2CConfig) {
	// Default I2C bus speed is 100 kHz.
	if config.Frequency == 0 {
		config.Frequency = TWI_FREQ_100KHZ
	}
	// Default I2C pins if not set.
	if config.SDA == 0 && config.SCL == 0 {
		config.SDA = SDA_PIN
		config.SCL = SCL_PIN
	}

	// do config
	sclPort, sclPin := config.SCL.getPortPin()
	sclPort.PIN_CNF[sclPin].Set((nrf.GPIO_PIN_CNF_DIR_Input << nrf.GPIO_PIN_CNF_DIR_Pos) |
		(nrf.GPIO_PIN_CNF_INPUT_Connect << nrf.GPIO_PIN_CNF_INPUT_Pos) |
		(nrf.GPIO_PIN_CNF_PULL_Pullup << nrf.GPIO_PIN_CNF_PULL_Pos) |
		(nrf.GPIO_PIN_CNF_DRIVE_S0D1 << nrf.GPIO_PIN_CNF_DRIVE_Pos) |
		(nrf.GPIO_PIN_CNF_SENSE_Disabled << nrf.GPIO_PIN_CNF_SENSE_Pos))

	sdaPort, sdaPin := config.SDA.getPortPin()
	sdaPort.PIN_CNF[sdaPin].Set((nrf.GPIO_PIN_CNF_DIR_Input << nrf.GPIO_PIN_CNF_DIR_Pos) |
		(nrf.GPIO_PIN_CNF_INPUT_Connect << nrf.GPIO_PIN_CNF_INPUT_Pos) |
		(nrf.GPIO_PIN_CNF_PULL_Pullup << nrf.GPIO_PIN_CNF_PULL_Pos) |
		(nrf.GPIO_PIN_CNF_DRIVE_S0D1 << nrf.GPIO_PIN_CNF_DRIVE_Pos) |
		(nrf.GPIO_PIN_CNF_SENSE_Disabled << nrf.GPIO_PIN_CNF_SENSE_Pos))

	if config.Frequency == TWI_FREQ_400KHZ {
		i2c.Bus.FREQUENCY.Set(nrf.TWI_FREQUENCY_FREQUENCY_K400)
	} else {
		i2c.Bus.FREQUENCY.Set(nrf.TWI_FREQUENCY_FREQUENCY_K100)
	}

	i2c.Bus.ENABLE.Set(nrf.TWI_ENABLE_ENABLE_Enabled)
	i2c.setPins(config.SCL, config.SDA)
}

// Tx does a single I2C transaction at the specified address.
// It clocks out the given address, writes the bytes in w, reads back len(r)
// bytes and stores them in r, and generates a stop condition on the bus.
func (i2c I2C) Tx(addr uint16, w, r []byte) (err error) {
	i2c.Bus.ADDRESS.Set(uint32(addr))

	if len(w) != 0 {
		i2c.Bus.TASKS_STARTTX.Set(1) // start transmission for writing
		for _, b := range w {
			if err = i2c.writeByte(b); err != nil {
				goto cleanUp
			}
		}
	}
	if len(r) != 0 {
		// To trigger suspend task when a byte is received
		i2c.Bus.SHORTS.Set(nrf.TWI_SHORTS_BB_SUSPEND)
		i2c.Bus.TASKS_STARTRX.Set(1) // re-start transmission for reading
		for i := range r {           // read each char
			if i+1 == len(r) {
				// To trigger stop task when last byte is received, set before resume task.
				i2c.Bus.SHORTS.Set(nrf.TWI_SHORTS_BB_STOP)
			}
			i2c.Bus.TASKS_RESUME.Set(1) // re-start transmission for reading
			if r[i], err = i2c.readByte(); err != nil {
				// goto/break are practically equivalent here,
				// but goto makes this more easily understandable for maintenance.
				goto cleanUp
			}
		}
	}

cleanUp:
	i2c.signalStop()
	i2c.Bus.SHORTS.Set(nrf.TWI_SHORTS_BB_SUSPEND_Disabled)
	return
}

// signalStop sends a stop signal when writing or tells the I2C peripheral that
// it must generate a stop condition after the next character is retrieved when
// reading.
func (i2c I2C) signalStop() {
	i2c.Bus.TASKS_STOP.Set(1)
	for i2c.Bus.EVENTS_STOPPED.Get() == 0 {
	}
	i2c.Bus.EVENTS_STOPPED.Set(0)
}

// writeByte writes a single byte to the I2C bus.
func (i2c I2C) writeByte(data byte) error {
	i2c.Bus.TXD.Set(uint32(data))
	for i2c.Bus.EVENTS_TXDSENT.Get() == 0 {
		if e := i2c.Bus.EVENTS_ERROR.Get(); e != 0 {
			i2c.Bus.EVENTS_ERROR.Set(0)
			return errI2CBusError
		}
	}
	i2c.Bus.EVENTS_TXDSENT.Set(0)
	return nil
}

// readByte reads a single byte from the I2C bus.
func (i2c I2C) readByte() (byte, error) {
	for i2c.Bus.EVENTS_RXDREADY.Get() == 0 {
		if e := i2c.Bus.EVENTS_ERROR.Get(); e != 0 {
			i2c.Bus.EVENTS_ERROR.Set(0)
			return 0, errI2CBusError
		}
	}
	i2c.Bus.EVENTS_RXDREADY.Set(0)
	return byte(i2c.Bus.RXD.Get()), nil
}

// SPI on the NRF.
type SPI struct {
	Bus *nrf.SPI_Type
}

// There are 2 SPI interfaces on the NRF5x.
var (
	SPI0 = SPI{Bus: nrf.SPI0}
	SPI1 = SPI{Bus: nrf.SPI1}
)

// SPIConfig is used to store config info for SPI.
type SPIConfig struct {
	Frequency uint32
	SCK       Pin
	SDO       Pin
	SDI       Pin
	LSBFirst  bool
	Mode      uint8
}

// Configure is intended to setup the SPI interface.
func (spi SPI) Configure(config SPIConfig) {
	// Disable bus to configure it
	spi.Bus.ENABLE.Set(nrf.SPI_ENABLE_ENABLE_Disabled)

	// set frequency
	var freq uint32

	if config.Frequency == 0 {
		config.Frequency = 4000000 // 4MHz
	}

	switch {
	case config.Frequency >= 8000000:
		freq = nrf.SPI_FREQUENCY_FREQUENCY_M8
	case config.Frequency >= 4000000:
		freq = nrf.SPI_FREQUENCY_FREQUENCY_M4
	case config.Frequency >= 2000000:
		freq = nrf.SPI_FREQUENCY_FREQUENCY_M2
	case config.Frequency >= 1000000:
		freq = nrf.SPI_FREQUENCY_FREQUENCY_M1
	case config.Frequency >= 500000:
		freq = nrf.SPI_FREQUENCY_FREQUENCY_K500
	case config.Frequency >= 250000:
		freq = nrf.SPI_FREQUENCY_FREQUENCY_K250
	default: // below 250kHz, default to the lowest speed available
		freq = nrf.SPI_FREQUENCY_FREQUENCY_K125
	}
	spi.Bus.FREQUENCY.Set(freq)

	var conf uint32

	// set bit transfer order
	if config.LSBFirst {
		conf = (nrf.SPI_CONFIG_ORDER_LsbFirst << nrf.SPI_CONFIG_ORDER_Pos)
	}

	// set mode
	switch config.Mode {
	case 0:
		conf &^= (nrf.SPI_CONFIG_CPOL_ActiveHigh << nrf.SPI_CONFIG_CPOL_Pos)
		conf &^= (nrf.SPI_CONFIG_CPHA_Leading << nrf.SPI_CONFIG_CPHA_Pos)
	case 1:
		conf &^= (nrf.SPI_CONFIG_CPOL_ActiveHigh << nrf.SPI_CONFIG_CPOL_Pos)
		conf |= (nrf.SPI_CONFIG_CPHA_Trailing << nrf.SPI_CONFIG_CPHA_Pos)
	case 2:
		conf |= (nrf.SPI_CONFIG_CPOL_ActiveLow << nrf.SPI_CONFIG_CPOL_Pos)
		conf &^= (nrf.SPI_CONFIG_CPHA_Leading << nrf.SPI_CONFIG_CPHA_Pos)
	case 3:
		conf |= (nrf.SPI_CONFIG_CPOL_ActiveLow << nrf.SPI_CONFIG_CPOL_Pos)
		conf |= (nrf.SPI_CONFIG_CPHA_Trailing << nrf.SPI_CONFIG_CPHA_Pos)
	default: // to mode
		conf &^= (nrf.SPI_CONFIG_CPOL_ActiveHigh << nrf.SPI_CONFIG_CPOL_Pos)
		conf &^= (nrf.SPI_CONFIG_CPHA_Leading << nrf.SPI_CONFIG_CPHA_Pos)
	}
	spi.Bus.CONFIG.Set(conf)

	// set pins
	spi.setPins(config.SCK, config.SDO, config.SDI)

	// Re-enable bus now that it is configured.
	spi.Bus.ENABLE.Set(nrf.SPI_ENABLE_ENABLE_Enabled)
}

// Transfer writes/reads a single byte using the SPI interface.
func (spi SPI) Transfer(w byte) (byte, error) {
	spi.Bus.TXD.Set(uint32(w))
	for spi.Bus.EVENTS_READY.Get() == 0 {
	}
	r := spi.Bus.RXD.Get()
	spi.Bus.EVENTS_READY.Set(0)

	// TODO: handle SPI errors
	return byte(r), nil
}

// Tx handles read/write operation for SPI interface. Since SPI is a syncronous write/read
// interface, there must always be the same number of bytes written as bytes read.
// The Tx method knows about this, and offers a few different ways of calling it.
//
// This form sends the bytes in tx buffer, putting the resulting bytes read into the rx buffer.
// Note that the tx and rx buffers must be the same size:
//
// 		spi.Tx(tx, rx)
//
// This form sends the tx buffer, ignoring the result. Useful for sending "commands" that return zeros
// until all the bytes in the command packet have been received:
//
// 		spi.Tx(tx, nil)
//
// This form sends zeros, putting the result into the rx buffer. Good for reading a "result packet":
//
// 		spi.Tx(nil, rx)
//
func (spi SPI) Tx(w, r []byte) error {
	var err error

	switch {
	case len(w) == 0:
		// read only, so write zero and read a result.
		for i := range r {
			r[i], err = spi.Transfer(0)
			if err != nil {
				return err
			}
		}
	case len(r) == 0:
		// write only
		spi.Bus.TXD.Set(uint32(w[0]))
		w = w[1:]
		for _, b := range w {
			spi.Bus.TXD.Set(uint32(b))
			for spi.Bus.EVENTS_READY.Get() == 0 {
			}
			spi.Bus.EVENTS_READY.Set(0)
			_ = spi.Bus.RXD.Get()
		}
		for spi.Bus.EVENTS_READY.Get() == 0 {
		}
		spi.Bus.EVENTS_READY.Set(0)
		_ = spi.Bus.RXD.Get()

	default:
		// write/read
		if len(w) != len(r) {
			return ErrTxInvalidSliceSize
		}

		for i, b := range w {
			r[i], err = spi.Transfer(b)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
