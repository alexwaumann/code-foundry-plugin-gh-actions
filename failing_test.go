package main

import "testing"

// A deliberately failing test, to exercise the plugin's failure path against a real
// GitHub Actions run. This branch is never merged.
func TestDeliberateFailure(t *testing.T) {
	got, want := 2+2, 5
	if got != want {
		t.Fatalf("2+2 = %d, want %d (deliberate failure for the gh-actions plugin's real run)", got, want)
	}
}
