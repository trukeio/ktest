# Pico CAN-FD + LIN interface

> **Status, September 2026: not on the rack's path.** For the bench, `ktestd`
> uses this board's MCP2518FD directly on the SPI header of an Orange Pi Zero 2,
> under the mainline `mcp251xfd` driver (outline §8,
> `examples/orangepi-zero2-mcp2518fd/`). That needs no firmware, and gives
> crystal-clocked stamps taken on the wire and transmit echoes from the
> controller's transmit event FIFO — more than a `gs_usb` device can carry,
> since `gs_usb` stamps are 32-bit microseconds.
>
> The Pico firmware remains worth building, for LIN and for a portable USB
> device, as a project of its own. For use with `ktestd` its `gs_usb` side
> should, beyond §5:
>
> - advertise `GS_CAN_FEATURE_HW_TIMESTAMP`, and run the MCP2518FD's
>   time-base counter at 1 MHz from the crystal;
> - stamp at one fixed point of the frame, start or end, and say which;
> - echo a transmitted frame from the transmit event FIFO with its stamp,
>   never on submission;
> - answer the driver's timestamp request with the counter;
> - report error frames and bus-off;
> - declare the controller's real clock and bit-timing constants, so that the
>   kernel's bit-timing calculation is made against the truth.
>
> Hardware timestamping, listed as optional in §7, is then a requirement.

## 1. Objective

Develop a low-cost USB automotive bus interface based on a **Raspberry Pi Pico / RP2040**, providing:

- 1 × CAN-FD channel
- 1 × LIN channel
- Linux compatibility
- Native Linux SocketCAN support for CAN
- Open firmware and hardware
- No proprietary PC-side SDK
- Suitable for automotive development, diagnostics and laboratory testing

The design should build on the existing Pico + MCP2518FD CAN-FD board.

## 2. Architecture

```text
                         USB
                          │
                    ┌─────┴─────┐
                    │ Raspberry  │
                    │ Pi Pico    │
                    │  RP2040    │
                    └──┬──────┬──┘
                       │      │
                     SPI    UART/PIO
                       │      │
                ┌──────▼──┐ ┌─▼──────────┐
                │ MCP2518FD│ │ LIN        │
                │ CAN-FD   │ │ transceiver│
                └──────┬───┘ └─────┬──────┘
                       │            │
                    CANH/L         LIN
```

## 3. CAN-FD subsystem

### Controller

**MCP2518FD**

- SPI connection to RP2040
- CAN 2.0 and CAN-FD
- Hardware acceptance filtering
- TX/RX FIFOs
- Interrupt to RP2040
- Existing board design can be reused

### Physical layer

Use a standard automotive CAN/CAN-FD transceiver.

Possible devices:

- MCP2562FD
- TCAN1044A/TCAN1042
- TJA1044GT or equivalent

Provide:

- CANH
- CANL
- optional 120 Ω termination
- optional termination-enable jumper/switch
- ESD protection
- automotive supply filtering

## 4. LIN subsystem

### Physical layer

Use a dedicated LIN transceiver, for example:

- TJA1021/TJA1027
- MCP2004A
- TLIN102x
- ATA662x family

The transceiver should provide:

- LIN bus interface
- TXD/RXD logic interface to RP2040
- automotive supply compatibility
- sleep/wake functionality where useful

### Protocol implementation

No dedicated LIN controller is required.

The RP2040 implements:

- LIN 1.x / 2.x framing
- break generation/detection
- sync field
- protected identifier
- data bytes
- classic/enhanced checksum
- master operation
- slave operation
- monitor/sniffer operation
- configurable LIN schedules

Use the RP2040 UART initially. Investigate PIO later if more precise timing or advanced LIN functionality is required.

## 5. USB interface

Prefer standard USB interfaces rather than a proprietary protocol.

### CAN

Expose CAN through **USB `gs_usb`**, allowing Linux to use the standard SocketCAN stack:

```text
USB → gs_usb → SocketCAN → can0
```

Example:

```bash
sudo ip link set can0 up type can bitrate 500000 dbitrate 2000000 fd on
candump can0
```

### LIN

Initially expose LIN through USB CDC ACM:

```text
USB → CDC ACM → /dev/ttyACM0
```

A small Linux userspace program can provide the LIN API.

The LIN interface should not be artificially represented as CAN frames. LIN has different semantics and should retain its own frame representation.

## 6. Linux software

Target Linux applications should require no vendor SDK.

Possible software layers:

```text
                    Application
                         │
                ┌────────┴────────┐
                │                 │
             SocketCAN         LIN API
                │                 │
              can0          /dev/ttyACM0
                │                 │
                └────── USB ──────┘
                         │
                       Pico
```

A future Go library can provide a unified abstraction:

```go
type CANFrame struct {
    ID   uint32
    Data []byte
}

type LINFrame struct {
    ID       uint8
    Data     []byte
    Checksum uint8
}
```

## 7. Hardware features

Recommended minimum hardware:

- RP2040 Pico/Pico-compatible module
- MCP2518FD
- CAN-FD transceiver
- LIN transceiver
- USB
- 5 V / 3.3 V power regulation as required
- CAN ESD protection
- LIN ESD protection
- CAN termination switch/jumper
- status LEDs

Useful optional features:

- galvanic isolation
- selectable CAN termination
- selectable LIN master pull-up configuration
- automotive connector
- 12 V automotive input
- wake/sleep control
- hardware timestamping
- second CAN channel

## 8. Firmware architecture

```text
┌───────────────────────────────────────┐
│              RP2040 firmware          │
├───────────────┬───────────────────────┤
│ USB subsystem │                       │
├───────────────┼───────────────────────┤
│ gs_usb        │ CDC LIN protocol     │
├───────────────┼───────────────────────┤
│ CAN driver    │ LIN driver            │
├───────────────┼───────────────────────┤
│ MCP2518FD     │ UART / PIO            │
└───────────────┴───────────────────────┘
```

The firmware should keep CAN and LIN drivers independent so that either subsystem can be developed and tested separately.

## 9. Development priorities

### Phase 1 — hardware

Reuse the existing MCP2518FD/Pico design and add:

1. LIN transceiver
2. LIN connector
3. protection
4. status LEDs
5. termination/configuration components

### Phase 2 — CAN

Reuse existing MCP2518FD firmware and implement/validate:

- classic CAN
- CAN-FD
- `gs_usb`
- SocketCAN interoperability

### Phase 3 — LIN

Implement:

1. LIN monitor
2. LIN master
3. LIN slave
4. checksum modes
5. configurable schedules

### Phase 4 — Linux tools

Create a small open-source command-line utility:

```text
pico-bus can ...
pico-bus lin ...
```

and a Go library for programmatic access.

## 10. Design philosophy

The interface should be:

**cheap, open, Linux-native and useful as a laboratory automotive tool.**

Avoid proprietary PC protocols wherever possible.

CAN should integrate directly into Linux as `can0` through `gs_usb`. LIN should expose its native semantics through a simple documented USB protocol.

The resulting device should be usable with existing Linux tools such as `ip`, `candump`, `cansend`, Wireshark and custom Go/Python applications without requiring manufacturer software.