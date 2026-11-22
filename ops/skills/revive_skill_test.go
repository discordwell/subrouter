package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What the revive skill tells an agent about resuming a run, retrying a stage,
// and a `stopped` list, read as text. revive-workflow-agents/test_wf_prefix.mjs
// runs the prefix itself.

var (
	datedRE    = regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`)
	resumeRE   = regexp.MustCompile(`(?i)\bresum`)
	retryRE    = regexp.MustCompile(`(?i)\bretr(?:y|ies|ied|ying)\b`)
	waitsRE    = regexp.MustCompile(`(?i)\bwaits?\b.*\bminutes?\b`)
	negationRE = regexp.MustCompile(`(?i)\b(?:never|not)\b`)
	stopRE     = regexp.MustCompile(`(?i)\bstop\b`)
	sentenceRE = regexp.MustCompile(`[.!?](?:\s+|$)`)
	functionRE = regexp.MustCompile(`(?m)^(?:async )?function (\w+)\(`)
)

func reviveSkill(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("revive-workflow-agents", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// section returns the text under a "## " heading, up to the next one.
func section(t *testing.T, text, heading string) string {
	t.Helper()
	marker := "\n## " + heading + "\n"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("SKILL.md has no section %q", heading)
	}
	body := text[start+len(marker):]
	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}
	return body
}

// sentences returns the skill's prose one sentence at a time, without its
// front matter, fenced code, or code spans.
func sentences(text string) []string {
	var out []string
	fences, inCode := 0, false
	for _, line := range strings.Split(text, "\n") {
		if fences < 2 {
			if line == "---" {
				fences++
			}
			continue
		}
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		for _, s := range sentenceRE.Split(spanRE.ReplaceAllString(line, ""), -1) {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestReviveSkillAllowsAResumeOnlyForARunOfOneUnit(t *testing.T) {
	text := reviveSkill(t)
	section(t, text, "Before anything: do not resume a run with several units")
	if strings.Contains(text, "maxActive") {
		t.Error("SKILL.md names maxActive: one agent at a time does not make a run of several units safe to resume")
	}
	for _, s := range sentences(text) {
		if resumeRE.MatchString(s) && !negationRE.MatchString(s) && !stopRE.MatchString(s) &&
			!datedRE.MatchString(s) && !strings.Contains(s, "one unit") {
			t.Errorf("a sentence speaks of a resume without forbidding it or limiting it to a run of one unit: %q", s)
		}
	}
}

func TestReviveSkillNeverAdvisesARetryAfterWaits(t *testing.T) {
	text := reviveSkill(t)
	for _, s := range sentences(text) {
		if negationRE.MatchString(s) || datedRE.MatchString(s) {
			continue
		}
		if retryRE.MatchString(s) {
			t.Errorf("a sentence speaks of a retry without forbidding it: %q", s)
		}
		if waitsRE.MatchString(s) && !strings.Contains(s, "probe") {
			t.Errorf("a sentence gives waits in minutes that are not between probes: %q", s)
		}
	}
	rule := section(t, text, "Make a script revivable")
	for _, want := range []string{"**A stage with no result stops its unit.**", "never retried with a fresh agent", "nothing is passed on"} {
		if !strings.Contains(rule, want) {
			t.Errorf("\"Make a script revivable\" does not say %q", want)
		}
	}
}

func TestReviveSkillExplainsEachPartOfThePrefix(t *testing.T) {
	rule := section(t, reviveSkill(t), "Make a script revivable")
	defined := map[string]bool{}
	for _, m := range functionRE.FindAllStringSubmatch(rule, -1) {
		defined[m[1]] = true
	}
	for _, name := range []string{"step", "unit", "finish"} {
		if !defined[name] {
			t.Errorf("the prefix defines no function %s", name)
		}
		if !strings.Contains(rule, "\n- `"+name+"(") {
			t.Errorf("no bullet under the prefix explains %s()", name)
		}
	}
	if !strings.Contains(rule, "\n- **The probe gate.**") {
		t.Error("no bullet under the prefix explains the probe gate")
	}
}

func TestReviveSkillSendsAStoppedListToTheChecksOfStepOne(t *testing.T) {
	text := reviveSkill(t)
	var stopped string
	for _, paragraph := range strings.Split(section(t, text, "When to use it"), "\n\n") {
		if strings.Contains(paragraph, "`stopped` list") && strings.Contains(paragraph, "step 1") {
			stopped = paragraph
		}
	}
	if stopped == "" {
		t.Fatal("\"When to use it\" has no paragraph that sends a `stopped` list to step 1")
	}
	for _, want := range []string{"daemon", "network", "revive", "Never give a stopped stage to a fresh agent"} {
		if !strings.Contains(stopped, want) {
			t.Errorf("the `stopped` paragraph does not say %q", want)
		}
	}
	var first string
	for _, line := range strings.Split(section(t, text, "Steps"), "\n") {
		if strings.HasPrefix(line, "1. ") {
			first = line
		}
	}
	for _, want := range []string{"/_subrouter/health", "https://api.anthropic.com"} {
		if !strings.Contains(first, want) {
			t.Errorf("step 1 does not check %s", want)
		}
	}
}
