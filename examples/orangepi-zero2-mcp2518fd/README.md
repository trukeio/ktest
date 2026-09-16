# MCP2518FD on an Orange Pi Zero 2

The bench's CAN interface (outline §8): an MCP2518FD board on the SPI header of
an Orange Pi Zero 2, under the mainline `mcp251xfd` driver. No firmware, no
out-of-tree code, and `ktestd` runs on the Zero 2 itself.

What it buys over a USB adapter: the controller stamps each frame from its own
counter, clocked by the board's crystal, as the frame is on the wire; and it
reports a transmitted frame from its transmit event FIFO once the frame has
actually been on the bus, with that stamp.

Not yet tried on a Zero 2. The overlay is derived from the kernel's H616 pin
table, the Zero 2 user manual's header table, and an overlay reported working
for the same chip and crystal on an Orange Pi Zero 3.

## Wiring

| MCP2518FD | Zero 2 pin | SoC |
|---|---|---|
| SCK | 23 | PH6, SPI1 |
| SDI (MOSI) | 19 | PH7 |
| SDO (MISO) | 21 | PH8 |
| nCS | 24 | PH9, SPI1 **chip select 1** |
| nINT | 22 | PC7 |
| 3.3 V | 1 or 17 | |
| GND | 20 or 25 | |

Pin 24 is chip select 1, not 0: the H616 puts SPI1's chip select 0 on PH5, which
the header gives to pin 3 as I2C3 SDA. So the device is `spi1.1`.

Check what the transceiver wants. An MCP2562FD, for one, takes 5 V on VDD
(pin 2 or 4) and 3.3 V on VIO; the Zero 2's GPIOs are 3.3 V only.

## Install

The crystal is 20 MHz; if yours is not, change `clock-frequency` in
`mcp2518fd-spi1.dts` first.

On Armbian:

    sudo armbian-add-overlay mcp2518fd-spi1.dts
    sudo reboot

On Orange Pi's own image:

    sudo dtc -@ -I dts -O dtb -o /boot/dtb/allwinner/overlay/sun50i-h616-mcp2518fd-spi1.dtbo mcp2518fd-spi1.dts
    # add mcp2518fd-spi1 to the overlays= line of /boot/orangepiEnv.txt
    sudo reboot

Do not also enable an SPI1 `spidev` overlay: it would claim the same chip select.

## Check

    dmesg | grep mcp251xfd

should end in something like

    mcp251xfd spi1.1 can0: MCP2518FD rev0.0 (... o:20.00MHz c:20.00MHz ... rs:8.50MHz ...) successfully initialized.

`rs:8.50MHz` is the SPI clock the driver allows itself: 0.85 × 20 MHz / 2. A
40 MHz crystal doubles it.

Bring it up, then read the rates back — `ip link` accepts a rate the divisors
cannot reach and runs the nearest one:

    sudo ip link set can0 up type can bitrate 500000 dbitrate 2000000 fd on
    ip -details link show can0 | grep -E 'bitrate|dbitrate'

Hardware timestamps:

    ethtool -T can0          # hardware-receive and hardware-raw-clock
    candump -H can0          # frames with the controller's stamps

A bus needs a second node to acknowledge frames; alone on a wire, every
transmission is an error. Termination: 120 Ω at each end of the bus, and
nowhere else.

## ktestd

Built for the Zero 2 on any machine:

    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/ktestd ./cmd/vcanprobe

Run `vcanprobe` against `can0` before anything relies on it (§8). `ktestd`
records software stamps today: `can/` asks the kernel for hardware stamps
but reads only the software one. Reading the hardware stamp, and saying in the
recording which clock each stream is on, is milestone 4's work (§12).
