package main

import (
	"fmt"
	"os"
	"time"
)

// frameSink is a transport that can write a raw wire frame. serialSink, netSink
// and bleSink all satisfy it; debugSink does not (OTA needs a real link).
type frameSink interface {
	write([]byte) error
}

const (
	otaChunk      = 1024            // firmware bytes per OTA_DATA frame (fits ota_buf)
	otaErasePause = 6 * time.Second // let esp_ota_begin erase the slot before streaming
)

// otaBeginFrame: length-prefixed [tokenLen, token, imageSize[4] LE].
func otaBeginFrame(token string, size uint32) []byte {
	inner := []byte{byte(len(token))}
	inner = append(inner, token...)
	inner = append(inner, byte(size), byte(size>>8), byte(size>>16), byte(size>>24))
	return frame(typeOTABegin, append([]byte{byte(len(inner))}, inner...))
}

// otaDataFrame: 16-bit length, then the chunk.
func otaDataFrame(chunk []byte) []byte {
	payload := make([]byte, 0, 2+len(chunk))
	payload = append(payload, byte(len(chunk)), byte(len(chunk)>>8))
	payload = append(payload, chunk...)
	return frame(typeOTAData, payload)
}

// otaEndFrame: length-prefixed imageSize[4] LE.
func otaEndFrame(size uint32) []byte {
	inner := []byte{byte(size), byte(size >> 8), byte(size >> 16), byte(size >> 24)}
	return frame(typeOTAEnd, append([]byte{byte(len(inner))}, inner...))
}

// runOTA streams a firmware .bin to the ESP32 over the chosen transport. The board
// writes it into the inactive OTA slot; esp_ota_end validates the image hash, so a
// corrupted/interrupted transfer just fails and leaves the running firmware intact.
func runOTA(s Sink, path, token string) error {
	fs, ok := s.(frameSink)
	if !ok {
		return fmt.Errorf("OTA needs a transport — use -serial, -net or -ble")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("%s is empty", path)
	}
	size := uint32(len(data))

	// Wait for the link to be up (net/ble connect in the background).
	for i := 0; i < 50 && !s.Alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !s.Alive() {
		return fmt.Errorf("transport not connected")
	}

	fmt.Printf("ota: %s (%d bytes) -> token %q\n", path, size, token)
	if err := fs.write(otaBeginFrame(token, size)); err != nil {
		return fmt.Errorf("send begin: %w", err)
	}
	fmt.Printf("ota: erasing slot (waiting %s) …\n", otaErasePause)
	time.Sleep(otaErasePause)

	fmt.Print("ota: sending ")
	nextPct := 10
	for off := 0; off < len(data); off += otaChunk {
		end := min(off+otaChunk, len(data))
		if err := fs.write(otaDataFrame(data[off:end])); err != nil {
			fmt.Println()
			return fmt.Errorf("send chunk at %d: %w", off, err)
		}
		if pct := end * 100 / len(data); pct >= nextPct {
			fmt.Printf("%d%% ", pct)
			nextPct += 10
		}
	}
	fmt.Println()

	if err := fs.write(otaEndFrame(size)); err != nil {
		return fmt.Errorf("send end: %w", err)
	}
	fmt.Println("ota: image sent — waiting for the board to validate and reboot …")

	// Success = the board accepted the image and rebooted, which drops this link
	// (TCP closed / serial node re-enumerates / BLE disconnects). If the link
	// stays up, the update did not apply — the reason is on the board's UART0 TX
	// (run a serial monitor: bad token, validate failed, size mismatch, …).
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Alive() {
			fmt.Println("ota: board rebooted — update applied ✓")
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("no reboot after 20s — update was NOT applied; " +
		"check the serial monitor for the \"OTA: …\" reason")
}
