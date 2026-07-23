package board

import "testing"

func TestParseTicketType(t *testing.T) {
	valid := []struct {
		in   string
		want TicketType
	}{
		{"", TypeFreeform},         // empty is valid → freeform (backward-compat)
		{"freeform", TypeFreeform}, // alias → normalises to the empty TypeFreeform
		{"research", TypeResearch},
		{"spec", TypeSpec},
		{"implement", TypeImplement},
		{"review", TypeReview},
	}
	for _, tt := range valid {
		got, err := ParseTicketType(tt.in)
		if err != nil {
			t.Errorf("ParseTicketType(%q) unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParseTicketType(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}

	invalid := []string{"RESEARCH", "impl", "plan", "Freeform", "unknown", " spec"}
	for _, s := range invalid {
		if _, err := ParseTicketType(s); err == nil {
			t.Errorf("ParseTicketType(%q) expected error; got nil", s)
		}
	}
}
