package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PATCHES §102.1, part H: §93.2 says which readings settle the latch of an
// automatic claim as the code settles it. The 10-second pass and a decision
// read a row only when its reading is fresh (autoResetReadingsOf), so a row
// served from last-good figures or from a throttled answer settles nothing.
// §93.2 once said every row the pass sees settles it, of any age.
const staleLatchRule = "every row the 10-second pass sees (of any age)"

const latchRule = "A row served from last-good figures or from a throttled answer is not fresh and settles nothing"

func TestPatchesSaysWhichRowsSettleALatchAsTheCodeDoes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "ops", "PATCHES.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "\n## 93. ")
	end := strings.Index(text, "\n## 94. ")
	if start < 0 || end < start {
		t.Fatal("ops/PATCHES.md has no §93 followed by §94")
	}
	section := text[start:end]
	if strings.Contains(section, staleLatchRule) {
		t.Errorf("§93.2 still says the latch settles on %q; the pass settles it only from fresh rows", staleLatchRule)
	}
	if !strings.Contains(section, latchRule) {
		t.Errorf("§93.2 does not say %q", latchRule)
	}
}
