package gitops

import (
	"strings"
	"testing"
)

const flagDoc = `# The single DR switch. Change it with drswitch, not by hand.
dr:
  # primary | dr
  activeSite: primary   # trailing comment survives
  standbyReplicas: 0
other:
  activeSite: "dr"
  quoted: 'primary'
`

func TestGetString(t *testing.T) {
	got, err := GetString([]byte(flagDoc), "dr.activeSite")
	if err != nil || got != "primary" {
		t.Fatalf("GetString = %q, %v", got, err)
	}
	if _, err := GetString([]byte(flagDoc), "dr.missing"); err == nil {
		t.Fatal("expected error for missing key")
	}
	if _, err := GetString([]byte(flagDoc), "dr"); err == nil {
		t.Fatal("expected error for non-scalar")
	}
}

func TestSetStringPreservesFormatting(t *testing.T) {
	out, err := SetString([]byte(flagDoc), "dr.activeSite", "dr")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(flagDoc, "activeSite: primary   #", "activeSite: dr   #", 1)
	if string(out) != want {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestSetStringQuotedStyles(t *testing.T) {
	out, err := SetString([]byte(flagDoc), "other.activeSite", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `activeSite: "primary"`) {
		t.Fatalf("double-quoted value not replaced:\n%s", out)
	}
	out, err = SetString(out, "other.quoted", "dr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `quoted: 'dr'`) {
		t.Fatalf("single-quoted value not replaced:\n%s", out)
	}
}

func TestSetStringRejectsUnsafeValues(t *testing.T) {
	if _, err := SetString([]byte(flagDoc), "dr.activeSite", "dr\nevil: true"); err == nil {
		t.Fatal("expected error for multi-line value")
	}
}
