package main

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// TestMain lets the test binary re-exec itself as the real vsockd entry
// point when VSOCKD_TEST_SUBPROCESS=1. Classic Go idiom for CLI smoke
// tests: avoids needing a separate `go build` step in CI.
func TestMain(m *testing.M) {
	if os.Getenv("VSOCKD_TEST_SUBPROCESS") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func runSubprocess(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "VSOCKD_TEST_SUBPROCESS=1")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("unexpected exec error: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func TestHelpExitsZero(t *testing.T) {
	_, _, code := runSubprocess(t, "-help")
	if code != 0 {
		t.Fatalf("-help exit code = %d, want 0", code)
	}
}

func TestVersionPrintsAndExitsZero(t *testing.T) {
	stdout, _, code := runSubprocess(t, "-version")
	if code != 0 {
		t.Fatalf("-version exit code = %d, want 0", code)
	}
	if stdout == "" {
		t.Fatalf("-version produced no stdout")
	}
}

func TestUnknownFlagExitsNonZero(t *testing.T) {
	_, _, code := runSubprocess(t, "-totally-unknown-flag")
	if code == 0 {
		t.Fatalf("unknown flag exit code = 0, want non-zero")
	}
}
