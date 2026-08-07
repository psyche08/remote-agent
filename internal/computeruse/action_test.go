package computeruse

import (
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func TestParseActionAcceptsValidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   ActionRequest
		want ActionID
	}{
		{"screenshot", ActionRequest{Action: "screen.capture"}, ActionScreenshot},
		{"move", ActionRequest{Action: "pointer.move", X: intPtr(10), Y: intPtr(20)}, ActionMove},
		{"click", ActionRequest{Action: "pointer.click", X: intPtr(1), Y: intPtr(2)}, ActionClick},
		{"scroll", ActionRequest{Action: "pointer.scroll", X: intPtr(1), Y: intPtr(2), DeltaY: -5}, ActionScroll},
		{"type", ActionRequest{Action: "keyboard.type", Text: "hello"}, ActionType},
		{"key", ActionRequest{Action: "keyboard.key", Keys: []string{"cmd", "s"}}, ActionKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAction(tc.in)
			if err != nil {
				t.Fatalf("ParseAction: %v", err)
			}
			if got.ID != tc.want {
				t.Fatalf("ID = %q, want %q", got.ID, tc.want)
			}
		})
	}
}

// The vocabulary is closed. An unrecognized action must be refused at the
// boundary rather than forwarded to the native helper.
func TestParseActionRejectsUnknownActions(t *testing.T) {
	for _, name := range []string{
		"", "shell.exec", "SCREEN.CAPTURE",
		"pointer.click; rm -rf /", "../../bin/sh", "screen.capture\x00",
	} {
		if _, err := ParseAction(ActionRequest{Action: name}); err == nil {
			t.Errorf("ParseAction(%q) was accepted; the action set must be closed", name)
		}
	}
	// Surrounding whitespace is trimmed, which does not widen the vocabulary:
	// the trimmed value still has to match a known id exactly.
	if _, err := ParseAction(ActionRequest{Action: "  screen.capture  "}); err != nil {
		t.Errorf("whitespace-padded known action rejected: %v", err)
	}
}

func TestParseActionRequiresCoordinates(t *testing.T) {
	for _, action := range []string{"pointer.move", "pointer.click", "pointer.scroll"} {
		if _, err := ParseAction(ActionRequest{Action: action}); err == nil {
			t.Errorf("%s accepted without coordinates", action)
		}
		if _, err := ParseAction(ActionRequest{Action: action, X: intPtr(1)}); err == nil {
			t.Errorf("%s accepted with only x", action)
		}
	}
	// A zero coordinate is a real screen position and must be accepted; this is
	// why the wire type uses pointers rather than treating 0 as unset.
	if _, err := ParseAction(ActionRequest{Action: "pointer.move", X: intPtr(0), Y: intPtr(0)}); err != nil {
		t.Errorf("(0,0) rejected: %v", err)
	}
}

func TestParseActionBoundsInputs(t *testing.T) {
	if _, err := ParseAction(ActionRequest{
		Action: "keyboard.type", Text: strings.Repeat("a", MaxTypeRunes+1),
	}); err == nil {
		t.Error("oversized type text accepted")
	}
	if _, err := ParseAction(ActionRequest{Action: "keyboard.type", Text: "a\x00b"}); err == nil {
		t.Error("type text containing NUL accepted")
	}
	if _, err := ParseAction(ActionRequest{
		Action: "keyboard.key", Keys: []string{"a", "b", "c", "d", "e", "f"},
	}); err == nil {
		t.Error("oversized key chord accepted")
	}
	if _, err := ParseAction(ActionRequest{
		Action: "pointer.click", X: intPtr(1), Y: intPtr(1), Count: 99,
	}); err == nil {
		t.Error("excessive click count accepted")
	}
	if _, err := ParseAction(ActionRequest{
		Action: "pointer.scroll", X: intPtr(1), Y: intPtr(1), DeltaY: MaxScrollMagnitude + 1,
	}); err == nil {
		t.Error("excessive scroll delta accepted")
	}
	if _, err := ParseAction(ActionRequest{
		Action: "pointer.move", X: intPtr(1 << 30), Y: intPtr(0),
	}); err == nil {
		t.Error("out-of-range coordinate accepted")
	}
}

func TestParseActionRejectsUnknownKeysAndButtons(t *testing.T) {
	if _, err := ParseAction(ActionRequest{
		Action: "keyboard.key", Keys: []string{"cmd", "nonsense"},
	}); err == nil {
		t.Error("unknown key name accepted")
	}
	if _, err := ParseAction(ActionRequest{
		Action: "pointer.click", X: intPtr(1), Y: intPtr(1), Button: "scroll-lock",
	}); err == nil {
		t.Error("unknown mouse button accepted")
	}
	// An empty button is the common default and means left.
	got, err := ParseAction(ActionRequest{Action: "pointer.click", X: intPtr(1), Y: intPtr(1)})
	if err != nil {
		t.Fatalf("default button: %v", err)
	}
	if got.Button != ButtonLeft || got.Count != 1 {
		t.Fatalf("defaults = %q/%d, want left/1", got.Button, got.Count)
	}
}

func TestParseActionRejectsEmptyScrollAndKeys(t *testing.T) {
	if _, err := ParseAction(ActionRequest{
		Action: "pointer.scroll", X: intPtr(1), Y: intPtr(1),
	}); err == nil {
		t.Error("scroll with no delta accepted")
	}
	if _, err := ParseAction(ActionRequest{Action: "keyboard.key"}); err == nil {
		t.Error("key action with no keys accepted")
	}
	if _, err := ParseAction(ActionRequest{Action: "keyboard.type"}); err == nil {
		t.Error("type action with no text accepted")
	}
}

// Nothing in the system layer may offer an unlock. Unlocking belongs to macOS
// and the Authorization Plug-in; a helper that could unlock directly would make
// every safeguard in the controller bypassable.
func TestActionCatalogHasNoUnlockOperation(t *testing.T) {
	for _, id := range ActionCatalog() {
		if strings.Contains(strings.ToLower(id), "unlock") {
			t.Errorf("action catalog exposes an unlock operation: %q", id)
		}
	}
	if len(ActionCatalog()) != 6 {
		t.Errorf("catalog has %d actions; update this test if the set changed", len(ActionCatalog()))
	}
}

func TestUnsupportedSystemFailsClosed(t *testing.T) {
	sys := unsupportedSystem{}
	if systemAvailable(sys) {
		t.Error("unsupported system reports itself available")
	}
	if _, err := sys.Locked(); err == nil {
		t.Error("Locked() succeeded on an unsupported platform")
	}
	if err := sys.Lock(); err == nil {
		t.Error("Lock() succeeded on an unsupported platform")
	}
	if _, err := sys.SinceLastInput(); err == nil {
		t.Error("SinceLastInput() succeeded on an unsupported platform")
	}
	if err := sys.Engage(); err == nil {
		t.Error("Engage() succeeded on an unsupported platform")
	}
	if sys.Engaged() {
		t.Error("Engaged() reported a shield on an unsupported platform")
	}
	if _, err := sys.Run(Action{ID: ActionScreenshot}); err == nil {
		t.Error("Run() succeeded on an unsupported platform")
	}
}
