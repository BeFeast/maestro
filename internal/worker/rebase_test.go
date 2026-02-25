package worker

import "testing"

func TestKeepBothSides_SimpleConflict(t *testing.T) {
	input := "line1\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> branch\nline2\n"
	got, changed, err := keepBothSides(input)
	if err != nil {
		t.Fatalf("keepBothSides: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := "line1\nours\ntheirs\nline2\n"
	if got != want {
		t.Fatalf("resolved content mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestKeepBothSides_Diff3Markers(t *testing.T) {
	input := "<<<<<<< HEAD\nours\n||||||| parent\nbase\n=======\ntheirs\n>>>>>>> branch\n"
	got, changed, err := keepBothSides(input)
	if err != nil {
		t.Fatalf("keepBothSides: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := "ours\ntheirs\n"
	if got != want {
		t.Fatalf("resolved content mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestKeepBothSides_NoConflict(t *testing.T) {
	input := "line1\nline2\n"
	got, changed, err := keepBothSides(input)
	if err != nil {
		t.Fatalf("keepBothSides: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false")
	}
	if got != input {
		t.Fatalf("content should remain unchanged\nwant:\n%s\n\ngot:\n%s", input, got)
	}
}

func TestKeepBothSides_UnterminatedConflict(t *testing.T) {
	input := "<<<<<<< HEAD\nours\n=======\ntheirs\n"
	_, _, err := keepBothSides(input)
	if err == nil {
		t.Fatal("expected error for unterminated conflict")
	}
}
