package domain

import "testing"

func TestParseStorageSizeValid(t *testing.T) {
	t.Parallel()

	tests := map[string]int64{
		"1Ki":  1024,
		"1Mi":  1 << 20,
		"2Gi":  2 << 30,
		"5Gi":  5 << 30,
		"10Gi": 10 << 30,
		"1Ti":  1 << 40,
		"500M": 500_000_000,
		"2G":   2_000_000_000,
		"1024": 1024,
		"0.5Gi": 1 << 29,
	}
	for raw, want := range tests {
		got, err := ParseStorageSize(raw)
		if err != nil {
			t.Fatalf("ParseStorageSize(%q) error: %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseStorageSize(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestParseStorageSizeInvalid(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"",
		"abc",
		"2GB",   // wrong suffix casing
		"2gi",   // wrong suffix casing
		"-1Gi",  // negative
		"+2Gi",  // sign not allowed
		"1 Gi",  // space
		"Gi",    // no number
		"0",     // zero
		"0Gi",   // zero
		"1.2.3Gi",
		"2Qi", // unknown suffix
		"2gI",
	}
	for _, raw := range invalid {
		if _, err := ParseStorageSize(raw); err == nil {
			t.Fatalf("ParseStorageSize(%q) = nil error, want rejection", raw)
		}
	}
}

func TestValidateStorageSizeWithin(t *testing.T) {
	t.Parallel()

	if err := ValidateStorageSizeWithin("10Gi", "10Gi"); err != nil {
		t.Fatalf("equal to max should pass: %v", err)
	}
	if err := ValidateStorageSizeWithin("10240Mi", "10Gi"); err != nil {
		t.Fatalf("10240Mi == 10Gi should pass: %v", err)
	}
	err := ValidateStorageSizeWithin("11Gi", "10Gi")
	if err == nil {
		t.Fatal("over max should fail")
	}
	// Misconfigured platform max must fail loudly, not silently accept.
	if err := ValidateStorageSizeWithin("1Gi", "banana"); err == nil {
		t.Fatal("invalid max should fail")
	}
}
