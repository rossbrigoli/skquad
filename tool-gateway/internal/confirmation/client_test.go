package confirmation

import "testing"

func TestArgsHashStability(t *testing.T) {
	h1 := ArgsHash("res-1", "rest_call", []byte(`{"a":1}`))
	h2 := ArgsHash("res-1", "rest_call", []byte(`{"a":1}`))
	if h1 != h2 {
		t.Fatalf("same inputs must hash identically: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("want 64 hex chars, got %d", len(h1))
	}

	// Payload change ⇒ different hash.
	if ArgsHash("res-1", "rest_call", []byte(`{"a":2}`)) == h1 {
		t.Fatal("payload change must change the hash")
	}
	// Resource change ⇒ different hash.
	if ArgsHash("res-2", "rest_call", []byte(`{"a":1}`)) == h1 {
		t.Fatal("resource change must change the hash")
	}
	// Operation change ⇒ different hash.
	if ArgsHash("res-1", "mcp_call", []byte(`{"a":1}`)) == h1 {
		t.Fatal("operation change must change the hash")
	}
	// Separator-collision: field-boundary shift must NOT collide.
	// ("ab"+"c") vs ("a"+"bc") with identical total payload.
	a := ArgsHash("ab", "c", []byte("x"))
	b := ArgsHash("a", "bc", []byte("x"))
	if a == b {
		t.Fatal("length-prefixing must prevent field-boundary collisions")
	}
	// Empty payload is stable and distinct from any payload.
	if ArgsHash("res-1", "op", nil) == ArgsHash("res-1", "op", []byte("y")) {
		t.Fatal("empty vs non-empty payload must differ")
	}
	if ArgsHash("res-1", "op", nil) != ArgsHash("res-1", "op", []byte{}) {
		t.Fatal("nil and empty payload must hash the same")
	}
}
