package app

import (
	"bytes"
	"errors"
	"testing"
)

func TestDoctorFindingsAreDeterministicAndOrderedBySeverity(t *testing.T) {
	var output bytes.Buffer
	state := doctorState{output: Streams{Stdout: &output}}
	state.pass("Zulu", "pass")
	state.warn("Zulu", "warn")
	state.fail("Zulu", errors.New("failure"))
	state.pass("Alpha", "pass")
	state.fail("Alpha", errors.New("second failure"))
	state.fail("Alpha", errors.New("first failure"))
	state.warn("Alpha", "warn")

	state.writeFindings()

	want := "[FAIL] Alpha: first failure\n" +
		"[FAIL] Alpha: second failure\n" +
		"[FAIL] Zulu: failure\n" +
		"[WARN] Alpha: warn\n" +
		"[WARN] Zulu: warn\n" +
		"[PASS] Alpha: pass\n" +
		"[PASS] Zulu: pass\n"
	if output.String() != want {
		t.Fatalf("doctor findings:\n%s\nwant:\n%s", output.String(), want)
	}
}
