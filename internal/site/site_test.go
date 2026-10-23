package site

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Site
		wantErr bool
	}{
		{"primary", Primary, false},
		{" DR ", DR, false},
		{"Primary", Primary, false},
		{"secondary", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("Parse(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestOther(t *testing.T) {
	if Primary.Other() != DR || DR.Other() != Primary {
		t.Fatal("Other() should swap primary and dr")
	}
}

func TestParseFlag(t *testing.T) {
	if got, err := ParseFlag("both"); err != nil || got != Both {
		t.Fatalf("ParseFlag(both) = %q, %v", got, err)
	}
	if _, err := Parse("both"); err == nil {
		t.Fatal("both must not be a valid switch target")
	}
	if Both.Valid() {
		t.Fatal("Both.Valid() should be false")
	}
}
