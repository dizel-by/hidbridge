package main

// A few Linux keycodes we reference in code by name.
const (
	keyA = 30
	keyZ = 44

	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112
	btnSide   = 0x113
	btnExtra  = 0x114
)

// keyNames maps common Linux evdev keycodes to readable names (for logging).
var keyNames = map[uint16]string{
	1: "ESC", 2: "1", 3: "2", 4: "3", 5: "4", 6: "5", 7: "6", 8: "7", 9: "8",
	10: "9", 11: "0", 12: "-", 13: "=", 14: "BACKSPACE", 15: "TAB",
	16: "Q", 17: "W", 18: "E", 19: "R", 20: "T", 21: "Y", 22: "U", 23: "I",
	24: "O", 25: "P", 26: "[", 27: "]", 28: "ENTER", 29: "LEFTCTRL",
	30: "A", 31: "S", 32: "D", 33: "F", 34: "G", 35: "H", 36: "J", 37: "K",
	38: "L", 39: ";", 40: "'", 41: "`", 42: "LEFTSHIFT", 43: "\\",
	44: "Z", 45: "X", 46: "C", 47: "V", 48: "B", 49: "N", 50: "M", 51: ",",
	52: ".", 53: "/", 54: "RIGHTSHIFT", 55: "KP*", 56: "LEFTALT", 57: "SPACE",
	58: "CAPSLOCK", 59: "F1", 60: "F2", 61: "F3", 62: "F4", 63: "F5", 64: "F6",
	65: "F7", 66: "F8", 67: "F9", 68: "F10", 87: "F11", 88: "F12",
	97: "RIGHTCTRL", 100: "RIGHTALT",
	102: "HOME", 103: "UP", 104: "PAGEUP", 105: "LEFT", 106: "RIGHT",
	107: "END", 108: "DOWN", 109: "PAGEDOWN", 110: "INSERT", 111: "DELETE",
	125: "LEFTMETA", 126: "RIGHTMETA",
	140: "CALC",
}

// hidUsage maps a Linux keycode to a USB HID Usage ID (Usage Page 0x07,
// Keyboard/Keypad). 0 means "no mapping yet". This is the table we'll use
// when building HID reports for the ESP32.
var hidUsage = map[uint16]uint16{
	// Letters A-Z.
	30: 0x04, 48: 0x05, 46: 0x06, 32: 0x07, 18: 0x08, 33: 0x09, 34: 0x0A,
	35: 0x0B, 23: 0x0C, 36: 0x0D, 37: 0x0E, 38: 0x0F, 50: 0x10, 49: 0x11,
	24: 0x12, 25: 0x13, 16: 0x14, 19: 0x15, 31: 0x16, 20: 0x17, 22: 0x18,
	47: 0x19, 17: 0x1A, 45: 0x1B, 21: 0x1C, 44: 0x1D,
	// Numbers 1-0.
	2: 0x1E, 3: 0x1F, 4: 0x20, 5: 0x21, 6: 0x22, 7: 0x23, 8: 0x24, 9: 0x25,
	10: 0x26, 11: 0x27,
	// Control / whitespace.
	28: 0x28, 1: 0x29, 14: 0x2A, 15: 0x2B, 57: 0x2C,
	// Symbols.
	12: 0x2D, 13: 0x2E, 26: 0x2F, 27: 0x30, 43: 0x31, 39: 0x33, 40: 0x34,
	41: 0x35, 51: 0x36, 52: 0x37, 53: 0x38, 58: 0x39,
	// Function keys.
	59: 0x3A, 60: 0x3B, 61: 0x3C, 62: 0x3D, 63: 0x3E, 64: 0x3F, 65: 0x40,
	66: 0x41, 67: 0x42, 68: 0x43, 87: 0x44, 88: 0x45,
	// Navigation.
	110: 0x49, 102: 0x4A, 104: 0x4B, 111: 0x4C, 107: 0x4D, 109: 0x4E,
	106: 0x4F, 105: 0x50, 108: 0x51, 103: 0x52,
	// Modifiers (also tracked as modifier byte; usages here for completeness).
	29: 0xE0, 42: 0xE1, 56: 0xE2, 125: 0xE3,
	97: 0xE4, 54: 0xE5, 100: 0xE6, 126: 0xE7,
}

// modBit maps modifier keycodes to their bit in the HID keyboard report's
// modifier byte. These keys go into the modifier byte, not the key array.
var modBit = map[uint16]byte{
	29:  0x01, // LEFTCTRL
	42:  0x02, // LEFTSHIFT
	56:  0x04, // LEFTALT
	125: 0x08, // LEFTMETA
	97:  0x10, // RIGHTCTRL
	54:  0x20, // RIGHTSHIFT
	100: 0x40, // RIGHTALT
	126: 0x80, // RIGHTMETA
}

// mouseButtonBit maps evdev mouse button codes to their HID report bit.
var mouseButtonBit = map[uint16]byte{
	btnLeft:   0x01,
	btnRight:  0x02,
	btnMiddle: 0x04,
	btnSide:   0x08,
	btnExtra:  0x10,
}

func keyName(code uint16) string {
	if n, ok := keyNames[code]; ok {
		return n
	}
	switch code {
	case btnLeft:
		return "BTN_LEFT"
	case btnRight:
		return "BTN_RIGHT"
	case btnMiddle:
		return "BTN_MIDDLE"
	case btnSide:
		return "BTN_SIDE"
	case btnExtra:
		return "BTN_EXTRA"
	}
	return "?"
}

func valueName(v int32) string {
	switch v {
	case keyUp:
		return "up"
	case keyDown:
		return "down"
	case keyRepeat:
		return "repeat"
	}
	return "?"
}

func isButton(code uint16) bool {
	return code >= btnLeft && code <= btnExtra
}
