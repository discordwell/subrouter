package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	keyCtrlC     = 0x03
	keyCtrlD     = 0x04
	keyBackspace = 0x08
	keyEnter     = '\r'
	keyNewline   = '\n'
	keyEscape    = 0x1b
	keyDelete    = 0x7f
)

// escapeSequenceGrace is how long a lone ESC waits for the rest of a sequence.
// Arrow keys and every other special key arrive as ESC followed immediately by
// more bytes, so a byte that shows up within this window means the user pressed
// a key rather than Escape itself. Terminals emit those bytes in one write, so
// the window only has to outlast scheduling, not typing.
const escapeSequenceGrace = 50 * time.Millisecond

// promptSwitchSelection reads the account picker's answer, returning cancelled
// when the user pressed Escape (or Ctrl-C, or Ctrl-D on an empty line) instead
// of committing to a switch.
//
// Escape needs raw mode: the cooked terminal hands the process nothing until
// Enter, so a plain ReadString cannot see the key at all. Anything that is not
// an interactive terminal — a pipe, a test, `</dev/null` — keeps the old
// line-based read, where EOF is already an empty answer and switches nothing.
func promptSwitchSelection(
	out io.Writer,
	reader *bufio.Reader,
	input io.Reader,
	prompt string,
) (string, bool, error) {
	file, isFile := input.(*os.File)
	if !isFile || reader.Buffered() != 0 || !term.IsTerminal(int(file.Fd())) {
		answer, err := promptLine(out, reader, prompt)
		return answer, false, err
	}
	state, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		// A terminal that will not go raw still deserves a working prompt; it
		// just cannot offer Escape.
		answer, err := promptLine(out, reader, prompt)
		return answer, false, err
	}
	defer func() { _ = term.Restore(int(file.Fd()), state) }()

	fmt.Fprint(out, prompt)
	answer, cancelled := readSwitchSelection(out, streamKeys(file))
	// Raw mode turns off output post-processing, so a bare \n would drop a line
	// without returning the carriage and print the next line mid-screen.
	fmt.Fprint(out, "\r\n")
	return answer, cancelled, nil
}

// streamKeys feeds one byte at a time onto a channel, so the reader below can
// wait on a byte with a timeout — which is the only way to tell a bare Escape
// from the ESC that opens an arrow key's sequence.
func streamKeys(input io.Reader) <-chan byte {
	keys := make(chan byte, 64)
	go func() {
		defer close(keys)
		buf := make([]byte, 1)
		for {
			n, err := input.Read(buf)
			if n > 0 {
				keys <- buf[0]
			}
			if err != nil {
				return
			}
		}
	}()
	return keys
}

// readSwitchSelection is the raw-mode key loop, kept separate from the terminal
// so it can be tested without a pty.
func readSwitchSelection(out io.Writer, keys <-chan byte) (string, bool) {
	var typed []byte
	for {
		key, ok := <-keys
		if !ok {
			// The terminal closed under us. Treat it as an empty answer, which
			// switches nothing, rather than as a cancel worth reporting.
			return string(typed), false
		}
		switch key {
		case keyEnter, keyNewline:
			return strings.TrimSpace(string(typed)), false
		case keyCtrlC:
			return "", true
		case keyCtrlD:
			if len(typed) == 0 {
				return "", true
			}
		case keyBackspace, keyDelete:
			if len(typed) > 0 {
				typed = typed[:len(typed)-1]
				// Erase the character under the cursor, not just move over it.
				fmt.Fprint(out, "\b \b")
			}
		case keyEscape:
			if consumeEscapeSequence(keys) {
				continue
			}
			return "", true
		default:
			if key >= 0x20 && key < 0x7f {
				typed = append(typed, key)
				// Raw mode echoes nothing, so the prompt has to draw itself.
				fmt.Fprint(out, string(rune(key)))
			}
		}
	}
}

// consumeEscapeSequence reports whether the ESC just read was the start of a
// key sequence rather than the Escape key, swallowing the sequence if so.
func consumeEscapeSequence(keys <-chan byte) bool {
	timer := time.NewTimer(escapeSequenceGrace)
	defer timer.Stop()
	select {
	case next, ok := <-keys:
		if !ok {
			return false
		}
		if next != '[' && next != 'O' {
			// Alt-<key> and other two-byte forms: swallowed, not a cancel.
			return true
		}
	case <-timer.C:
		return false
	}
	// CSI parameter bytes run until a final byte in @..~.
	for {
		next, ok := <-keys
		if !ok || (next >= '@' && next <= '~') {
			return true
		}
	}
}
