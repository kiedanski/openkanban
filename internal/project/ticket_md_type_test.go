package project

import (
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

// TestTicketTypeRoundTrips checks that a first-class Type survives a
// marshal→unmarshal cycle for every value, and that empty parses back as
// freeform.
func TestTicketTypeRoundTrips(t *testing.T) {
	for _, typ := range []board.TicketType{
		board.TypeFreeform,
		board.TypeResearch,
		board.TypeSpec,
		board.TypeImplement,
		board.TypeReview,
	} {
		orig := board.NewTicket("Typed ticket", "proj-test")
		orig.Type = typ

		data, err := MarshalTicket(orig)
		if err != nil {
			t.Fatalf("MarshalTicket(%q): %v", typ, err)
		}
		got, err := UnmarshalTicket(data)
		if err != nil {
			t.Fatalf("UnmarshalTicket(%q): %v\n%s", typ, err, data)
		}
		if got.Type != typ {
			t.Errorf("round-trip Type = %q, want %q", got.Type, typ)
		}
	}
}

// TestTicketTypeMissingIsFreeform pins backward-compat: a .md written before
// the Type field existed (no `type:` line) loads as freeform, not an error.
func TestTicketTypeMissingIsFreeform(t *testing.T) {
	md := "---\nid: t-legacy\ntitle: Legacy\nstatus: backlog\n---\n\nbody\n"
	got, err := UnmarshalTicket([]byte(md))
	if err != nil {
		t.Fatalf("UnmarshalTicket: %v", err)
	}
	if got.Type != board.TypeFreeform {
		t.Errorf("missing type = %q, want freeform (empty)", got.Type)
	}
}

// TestTicketTypeUnknownRejected pins that a present-but-invalid type is a
// hard error (a hand-edit typo surfaces instead of silently dropping).
func TestTicketTypeUnknownRejected(t *testing.T) {
	md := "---\nid: t-bad\ntitle: Bad\nstatus: backlog\ntype: implment\n---\n\nbody\n"
	_, err := UnmarshalTicket([]byte(md))
	if err == nil {
		t.Fatal("UnmarshalTicket accepted an invalid type; want error")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error %q should mention the offending field 'type'", err)
	}
}
