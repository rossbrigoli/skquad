package grantlint

import (
	"testing"
)

func codes(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Code
	}
	return out
}

func hasCode(fs []Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

// --- known-bad corpus: each code triggered ---

func TestCorpusNewCredentialedReach(t *testing.T) {
	before := &ChangeSnapshot{Hosts: []string{"api.github.com"}, HasCredential: false}
	after := &ChangeSnapshot{Hosts: []string{"api.github.com", "gitlab.com"}, HasCredential: true}
	fs := LintChange(before, after)
	if !hasCode(fs, CodeNewCredentialedReach) {
		t.Fatalf("expected new_credentialed_reach, got %v", codes(fs))
	}
}

func TestCorpusNewCredentialedReachNoGainClean(t *testing.T) {
	// Credential added but NO new hosts: reach unchanged.
	before := &ChangeSnapshot{Hosts: []string{"api.github.com"}, HasCredential: false}
	after := &ChangeSnapshot{Hosts: []string{"api.github.com"}, HasCredential: true}
	fs := LintChange(before, after)
	if hasCode(fs, CodeNewCredentialedReach) {
		t.Fatalf("unexpected new_credentialed_reach, got %v", codes(fs))
	}
}

func TestCorpusNewHTTPMethod(t *testing.T) {
	before := &ChangeSnapshot{Hosts: []string{"x.example.com"}, HTTPMethods: []string{"GET"}}
	after := &ChangeSnapshot{Hosts: []string{"x.example.com"}, HTTPMethods: []string{"GET", "POST"}}
	fs := LintChange(before, after)
	if !hasCode(fs, CodeNewHTTPMethod) {
		t.Fatalf("expected new_http_method, got %v", codes(fs))
	}
}

func TestCorpusNewMCPTool(t *testing.T) {
	before := &ChangeSnapshot{MCPTools: []string{"browser.navigate"}}
	after := &ChangeSnapshot{MCPTools: []string{"browser.navigate", "browser.click"}}
	fs := LintChange(before, after)
	if !hasCode(fs, CodeNewMCPTool) {
		t.Fatalf("expected new_mcp_tool, got %v", codes(fs))
	}
}

func TestCorpusMetadataPathAbsolute(t *testing.T) {
	// metadata_path fires EVEN WHEN PRESENT IN BEFORE (absolute rule).
	host := "169.254.169.254"
	before := &ChangeSnapshot{Hosts: []string{host}}
	after := &ChangeSnapshot{Hosts: []string{host}}
	fs := LintChange(before, after)
	if !hasCode(fs, CodeMetadataPath) {
		t.Fatalf("metadata_path must fire even when unchanged, got %v", codes(fs))
	}
}

func TestCorpusMetadataHostnames(t *testing.T) {
	for _, h := range []string{
		"metadata.google.internal",
		"foo.metadata.goog",
		"http://metadata.google.internal/computeMetadata/v1/",
		"169.254.169.254",
		"100.100.100.200",
	} {
		fs := LintChange(nil, &ChangeSnapshot{Hosts: []string{h}})
		if !hasCode(fs, CodeMetadataPath) {
			t.Errorf("expected metadata_path for %q, got %v", h, codes(fs))
		}
	}
}

func TestCorpusClusterInternalHosts(t *testing.T) {
	for _, h := range []string{
		"10.1.2.3",
		"172.20.0.4",
		"192.168.68.131",
		"100.64.5.6",
		"fd12::34",
		"skquad-api-server.skquad-system.svc",
		"foo.svc.cluster.local",
		"bar.cluster.local",
		"db.internal",
		"http://skquad-tool-gateway.skquad-system.svc.cluster.local:8080/x",
	} {
		fs := LintChange(nil, &ChangeSnapshot{Hosts: []string{h}})
		if !hasCode(fs, CodeClusterInternalPath) {
			t.Errorf("expected cluster_internal_path for %q, got %v", h, codes(fs))
		}
	}
}

func TestCorpusEgressClassNewlyInternal(t *testing.T) {
	before := &ChangeSnapshot{EgressClass: "public"}
	after := &ChangeSnapshot{EgressClass: "internal"}
	fs := LintChange(before, after)
	if !hasCode(fs, CodeClusterInternalPath) {
		t.Fatalf("expected cluster_internal_path for newly-internal egress, got %v", codes(fs))
	}
	// Already internal before → not gained → clean.
	fs = LintChange(&ChangeSnapshot{EgressClass: "internal"}, &ChangeSnapshot{EgressClass: "internal"})
	if hasCode(fs, CodeClusterInternalPath) {
		t.Fatalf("unchanged internal egress must be clean, got %v", codes(fs))
	}
}

func TestCorpusCeilingWidened(t *testing.T) {
	before := &ChangeSnapshot{NumericCaps: map[string]int{"max_pages": 50, "max_request_bytes": 65536}}
	after := &ChangeSnapshot{NumericCaps: map[string]int{"max_pages": 100, "max_request_bytes": 65536}}
	fs := LintChange(before, after)
	if !hasCode(fs, CodeCeilingWidened) {
		t.Fatalf("expected ceiling_widened, got %v", codes(fs))
	}
	// Shrink is clean.
	fs = LintChange(after, before)
	if hasCode(fs, CodeCeilingWidened) {
		t.Fatalf("narrowing must be clean, got %v", codes(fs))
	}
}

func TestCorpusCombinedBadShape(t *testing.T) {
	before := &ChangeSnapshot{
		Hosts:       []string{"api.github.com"},
		HTTPMethods: []string{"GET"},
	}
	after := &ChangeSnapshot{
		Hosts:         []string{"api.github.com", "gitlab.com", "169.254.169.254", "db.internal"},
		HTTPMethods:   []string{"GET", "PUT"},
		MCPTools:      []string{"shell.exec"},
		HasCredential: true,
		NumericCaps:   map[string]int{"rate": 10},
	}
	fs := LintChange(before, after)
	want := []string{
		CodeNewCredentialedReach,
		CodeNewHTTPMethod,
		CodeNewMCPTool,
		CodeMetadataPath,
		CodeClusterInternalPath,
	}
	for _, w := range want {
		if !hasCode(fs, w) {
			t.Errorf("combined shape missing %s; got %v", w, codes(fs))
		}
	}
	if !HasBlock(fs) {
		t.Error("combined shape must contain block severity")
	}
}

// --- known-good corpus: zero findings ---

func TestCorpusPureNarrowingClean(t *testing.T) {
	before := &ChangeSnapshot{
		Hosts:         []string{"a.example.com", "b.example.com"},
		HTTPMethods:   []string{"GET", "POST"},
		MCPTools:      []string{"t1", "t2"},
		HasCredential: true,
		NumericCaps:   map[string]int{"max_pages": 100},
		EgressClass:   "public",
	}
	after := &ChangeSnapshot{
		Hosts:         []string{"a.example.com"},
		HTTPMethods:   []string{"GET"},
		MCPTools:      []string{"t1"},
		HasCredential: true,
		NumericCaps:   map[string]int{"max_pages": 50},
		EgressClass:   "public",
	}
	fs := LintChange(before, after)
	if len(fs) != 0 {
		t.Fatalf("pure narrowing must be clean, got %v", codes(fs))
	}
}

func TestCorpusIdenticalClean(t *testing.T) {
	s := &ChangeSnapshot{
		Hosts:         []string{"api.example.com"},
		HTTPMethods:   []string{"GET"},
		HasCredential: true,
		EgressClass:   "public",
		NumericCaps:   map[string]int{"rate": 5},
	}
	fs := LintChange(s, s)
	if len(fs) != 0 {
		t.Fatalf("identical snapshots must be clean, got %v", codes(fs))
	}
}

func TestCorpusBYOSelfGrantSameShapePublicGETClean(t *testing.T) {
	// BYO self-grant of a plain public GET resource: brand-new (before=nil)
	// but nothing dangerous.
	after := &ChangeSnapshot{
		RiskTier:      "low",
		Hosts:         []string{"api.github.com"},
		HTTPMethods:   []string{"GET"},
		HasCredential: true,
		EgressClass:   "public",
	}
	fs := LintChange(nil, after)
	if len(fs) != 0 {
		t.Fatalf("BYO public GET self-grant must be clean, got %v", codes(fs))
	}
}

func TestNilAfterClean(t *testing.T) {
	fs := LintChange(&ChangeSnapshot{Hosts: []string{"x.example.com"}}, nil)
	if len(fs) != 0 {
		t.Fatalf("nil after = no change, got %v", codes(fs))
	}
}

// --- determinism ---

func TestDeterminism(t *testing.T) {
	before := &ChangeSnapshot{Hosts: []string{"a.com"}, HTTPMethods: []string{"GET"}, NumericCaps: map[string]int{"rate": 1}}
	after := &ChangeSnapshot{
		Hosts:         []string{"a.com", "b.com", "c.internal", "169.254.170.1"},
		HTTPMethods:   []string{"GET", "POST", "DELETE"},
		MCPTools:      []string{"z.tool", "a.tool"},
		HasCredential: true,
		NumericCaps:   map[string]int{"rate": 9, "max_pages": 3},
	}
	first := LintChange(before, after)
	for i := 0; i < 25; i++ {
		got := LintChange(before, after)
		if len(got) != len(first) {
			t.Fatalf("run %d: length drift %d vs %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d finding %d: %+v vs %+v", i, j, got[j], first[j])
			}
		}
	}
	// Sorted by Code then Detail.
	for j := 1; j < len(first); j++ {
		if first[j-1].Code > first[j].Code ||
			(first[j-1].Code == first[j].Code && first[j-1].Detail > first[j].Detail) {
			t.Fatalf("findings not sorted at %d: %+v", j, first)
		}
	}
}

func TestUnparseableHostConservative(t *testing.T) {
	// Garbage host strings: no IP parsing attempted; suffix rules still apply
	// and nothing panics. A non-matching garbage host yields no internal finding.
	fs := LintChange(nil, &ChangeSnapshot{Hosts: []string{"!!!not-a-host"}})
	if hasCode(fs, CodeClusterInternalPath) {
		t.Fatalf("non-matching garbage host must not trigger internal finding, got %v", codes(fs))
	}
	fs = LintChange(nil, &ChangeSnapshot{Hosts: []string{"!!!weird.internal"}})
	if !hasCode(fs, CodeClusterInternalPath) {
		t.Fatalf("suffix rule must still apply to non-IP host, got %v", codes(fs))
	}
}
