package domain

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// storageQuantityRe matches Kubernetes-style resource quantity strings as
// accepted for agent workspace PVC sizes (S-138): a positive decimal
// number with an optional binary (Ki/Mi/Gi/Ti/Pi/Ei) or decimal
// (K/M/G/T/P/E) suffix. go.mod carries no k8s.io/apimachinery
// dependency, so this is the deliberately-scoped stand-in for
// resource.Quantity parsing. Signs, spaces, and unknown suffixes are
// rejected.
var storageQuantityRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)?$`)

// storageMultipliers maps quantity suffixes to their byte multipliers.
var storageMultipliers = map[string]float64{
	"":  1,
	"K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60,
}

// ParseStorageSize parses a Kubernetes-style storage quantity (e.g.
// "1Gi", "2Gi", "500M") and returns its size in bytes. It errors on
// malformed input and on non-positive values.
func ParseStorageSize(raw string) (int64, error) {
	m := storageQuantityRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("invalid storage_size %q: expected a positive quantity like 1Gi, 2Gi or 500M", raw)
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid storage_size %q: %w", raw, err)
	}
	bytes := value * storageMultipliers[m[2]]
	if bytes <= 0 {
		return 0, errors.New("storage_size must be positive")
	}
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("storage_size %q overflows int64 bytes", raw)
	}
	return int64(bytes), nil
}

// ValidateStorageSizeWithin parses raw and checks it is > 0 and <= max
// (both Kubernetes-style quantity strings). maxRaw is validated too so
// a misconfigured platform max fails loudly rather than silently
// accepting oversized requests.
func ValidateStorageSizeWithin(raw, maxRaw string) error {
	sizeBytes, err := ParseStorageSize(raw)
	if err != nil {
		return err
	}
	maxBytes, err := ParseStorageSize(maxRaw)
	if err != nil {
		return fmt.Errorf("platform maximum storage size is misconfigured: %w", err)
	}
	if sizeBytes > maxBytes {
		return fmt.Errorf("storage_size %q exceeds the platform maximum %s", raw, maxRaw)
	}
	return nil
}
