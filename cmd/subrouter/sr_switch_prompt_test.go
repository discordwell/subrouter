package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func keyChannel(input string) <-chan byte {
	keys := make(chan byte, len(input)+1)
	for i := 0; i < len(input); i++ {
		keys <- input[i]
	}
	close(keys)
	return keys
}

func TestReadSwitchSelectionCancelsOnEscape(t *testing.T) {
	var out bytes.Buffer
	answer, cancelled := readSwitchSelection(&out, keyChannel("\x1b"))
	if !cancelled {
		t.Fatalf("Escape did not cancel the picker (answer %q)", answer)
	}
	if answer != "" {
		t.Fatalf("cancelled prompt returned %q, want empty", answer)
	}
}

// Escape typed after a digit must still cancel: the digit is abandoned, not
// submitted, or dismissing the prompt would switch accounts.
func TestReadSwitchSelectionEscapeAbandonsTypedInput(t *testing.T) {
	var out bytes.Buffer
	answer, cancelled := readSwitchSelection(&out, keyChannel("3\x1b"))
	if !cancelled || answer != "" {
		t.Fatalf("answer = %q, cancelled = %v, want empty and cancelled", answer, cancelled)
	}
}

// An arrow key is ESC + "[A". Reading its ESC as a cancel would make the arrow
// keys dismiss the prompt, which is the trap this disambiguation exists for.
func TestReadSwitchSelectionIgnoresArrowKeys(t *testing.T) {
	var out bytes.Buffer
	answer, cancelled := readSwitchSelection(&out, keyChannel("\x1b[A\x1b[B7\r"))
	if cancelled {
		t.Fatal("arrow keys cancelled the picker")
	}
	if answer != "7" {
		t.Fatalf("answer = %q, want 7", answer)
	}
}

func TestReadSwitchSelectionSubmitsOnEnter(t *testing.T) {
	var out bytes.Buffer
	answer, cancelled := readSwitchSelection(&out, keyChannel("12\r"))
	if cancelled {
		t.Fatal("Enter cancelled the picker")
	}
	if answer != "12" {
		t.Fatalf("answer = %q, want 12", answer)
	}
	if !strings.Contains(out.String(), "12") {
		t.Fatalf("raw mode echoes nothing, so the prompt must draw what was typed:\n%q", out.String())
	}
}

func TestReadSwitchSelectionBackspaceErasesTypedInput(t *testing.T) {
	var out bytes.Buffer
	answer, cancelled := readSwitchSelection(&out, keyChannel("19\x7f2\r"))
	if cancelled {
		t.Fatal("backspace cancelled the picker")
	}
	if answer != "12" {
		t.Fatalf("answer = %q, want 12", answer)
	}
	if !strings.Contains(out.String(), "\b \b") {
		t.Fatalf("backspace did not erase the character on screen:\n%q", out.String())
	}
}

func TestReadSwitchSelectionCancelsOnCtrlC(t *testing.T) {
	var out bytes.Buffer
	if _, cancelled := readSwitchSelection(&out, keyChannel("4\x03")); !cancelled {
		t.Fatal("Ctrl-C did not cancel the picker")
	}
}

// A closed terminal is an empty answer, which switches nothing. Reporting it as
// a cancel would be indistinguishable from Escape and hide a real hangup.
func TestReadSwitchSelectionTreatsClosedInputAsEmptyAnswer(t *testing.T) {
	var out bytes.Buffer
	answer, cancelled := readSwitchSelection(&out, keyChannel(""))
	if cancelled || answer != "" {
		t.Fatalf("answer = %q, cancelled = %v, want empty and not cancelled", answer, cancelled)
	}
}

// Everything that is not an interactive terminal keeps the line-based read, so
// pipes, tests and `sr </dev/null` behave exactly as before.
func TestPromptSwitchSelectionFallsBackForNonTerminalInput(t *testing.T) {
	var out bytes.Buffer
	input := strings.NewReader("5\n")
	answer, cancelled, err := promptSwitchSelection(&out, bufio.NewReader(input), input, "Switch to (#): ")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("a pipe cannot press Escape, so it must never report a cancel")
	}
	if answer != "5" {
		t.Fatalf("answer = %q, want 5", answer)
	}
	if !strings.Contains(out.String(), "Switch to (#): ") {
		t.Fatalf("fallback did not print the prompt:\n%q", out.String())
	}
}

func TestPromptSwitchSelectionTreatsEOFAsEmptyAnswer(t *testing.T) {
	var out bytes.Buffer
	input := strings.NewReader("")
	answer, cancelled, err := promptSwitchSelection(&out, bufio.NewReader(input), input, "Switch to (#): ")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled || answer != "" {
		t.Fatalf("answer = %q, cancelled = %v, want empty and not cancelled", answer, cancelled)
	}
}
