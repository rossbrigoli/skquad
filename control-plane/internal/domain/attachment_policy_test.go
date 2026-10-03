package domain

// S-216: attachment policy tests — the executable-rejection core.
// Fixtures are magic-byte-accurate headers; the validator only sniffs
// the leading bytes, so trailing filler is fine.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixture(magic []byte, pad int) []byte {
	return append(append([]byte{}, magic...), bytes.Repeat([]byte{0x11}, pad)...)
}

var (
	elfHeader   = []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	peHeader    = append(append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0x00}, 58)...), []byte("PE\x00\x00")...)
	macho32     = []byte{0xfe, 0xed, 0xfa, 0xce, 0x07, 0x00, 0x00, 0x01}
	macho64     = []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07, 0x00, 0x00, 0x01}
	machoFat    = []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x02}
	wasmHeader  = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	classHeader = []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0x00, 0x00, 0x34}
	pngHeader   = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	pdfHeader   = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	zipHeader   = []byte("PK\x03\x04")
	webpHead    = append(append([]byte("RIFF"), 0xe0, 0x00, 0x00, 0x00), []byte("WEBP")...)
	mp4Head     = append(append([]byte{0x00, 0x00, 0x00, 0x18}, []byte("ftypisom")...), bytes.Repeat([]byte{0x00}, 64)...)
	webmHead    = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x01, 0x00, 0x00, 0x00}
	oggHead     = append([]byte("OggS"), bytes.Repeat([]byte{0x00}, 64)...)
	flacHead    = append([]byte("fLaC"), bytes.Repeat([]byte{0x00}, 64)...)
)

func TestInspectInboxAttachment_RejectsExecutables(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		data     []byte
		why      string
	}{
		{"elf", "report.bin.txt", fixture(elfHeader, 128), "ELF"},
		{"elf named pdf", "quarterly.pdf", fixture(elfHeader, 128), "ELF"},
		{"pe", "invoice.doc", fixture(peHeader, 128), "PE/DOS"},
		{"macho32", "thing.txt", fixture(macho32, 128), "Mach-O"},
		{"macho64", "thing.txt", fixture(macho64, 128), "Mach-O"},
		{"macho fat", "thing.txt", fixture(machoFat, 128), "fat"},
		{"wasm", "module.dat", fixture(wasmHeader, 128), "WebAssembly"},
		{"java class", "Report.dat", fixture(classHeader, 128), "Java class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, rej := InspectInboxAttachment(tc.filename, tc.data)
			require.NotEmpty(t, rej, "expected rejection for %s", tc.filename)
			require.Empty(t, ct)
			require.Contains(t, rej, tc.why)
		})
	}
}

func TestInspectInboxAttachment_RejectsDangerousExtensions(t *testing.T) {
	for _, ext := range []string{".exe", ".DLL", ".so", ".dylib", ".bin", ".wasm", ".class", ".jar", ".msi", ".scr", ".com", ".o", ".a", ".ko", ".efi"} {
		name := "payload" + ext
		// Even benign text content with an executable extension is refused.
		ct, rej := InspectInboxAttachment(name, []byte("just some text, harmless"))
		require.NotEmpty(t, rej, "expected rejection for %s", name)
		require.Empty(t, ct)
		require.Contains(t, rej, "extension")
	}
}

func TestInspectInboxAttachment_AllowsInspectableFiles(t *testing.T) {
	cases := []struct {
		name        string
		filename    string
		data        []byte
		wantTypePfx string
	}{
		{"plain text", "notes.txt", []byte("hello squad, here is the report"), "text/"},
		{"shebang bash", "run.sh", []byte("#!/bin/bash\necho hi\n"), "text/"},
		{"python", "analysis.py", []byte("#!/usr/bin/env python3\nprint('hi')\n"), "text/"},
		{"javascript", "chart.js", []byte("console.log('hi');\n"), "text/"},
		{"csv", "data.csv", []byte("a,b,c\n1,2,3\n"), "text/"},
		{"markdown", "summary.md", []byte("# Report\n\nAll good.\n"), "text/"},
		{"json", "metrics.json", []byte("{\"ok\": true}"), "text/"},
		{"pdf", "report.pdf", fixture(pdfHeader, 256), "application/pdf"},
		{"png", "chart.png", fixture(pngHeader, 256), "image/png"},
		{"zip", "bundle.zip", fixture(zipHeader, 256), "application/zip"},
		{"webp", "photo.webp", append(append([]byte{}, webpHead...), bytes.Repeat([]byte{0x01}, 64)...), "image/webp"},
		{"mp4", "clip.mp4", mp4Head, "video/mp4"},
		{"webm", "clip.webm", fixture(webmHead, 128), "video/webm"},
		{"ogg", "sound.ogg", oggHead, "audio/ogg"},
		{"flac", "audio.flac", flacHead, "audio/flac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, rej := InspectInboxAttachment(tc.filename, tc.data)
			require.Empty(t, rej, "unexpected rejection: %s", rej)
			require.True(t, strings.HasPrefix(ct, tc.wantTypePfx), "got %q want prefix %q", ct, tc.wantTypePfx)
		})
	}
}

func TestInspectInboxAttachment_RejectsUnknownBinary(t *testing.T) {
	// Random-ish binary with NUL bytes that matches no known magic.
	data := append([]byte{0xde, 0xad, 0x00, 0x17, 0xbe, 0xef}, bytes.Repeat([]byte{0x00, 0x01}, 128)...)
	ct, rej := InspectInboxAttachment("mystery.dat", data)
	require.NotEmpty(t, rej)
	require.Empty(t, ct)
	require.Contains(t, rej, "uninspectable binary")
}

func TestInspectInboxAttachment_RejectsEmptyAndOversized(t *testing.T) {
	_, rej := InspectInboxAttachment("empty.txt", nil)
	require.Contains(t, rej, "empty")

	_, rej = InspectInboxAttachment("big.txt", bytes.Repeat([]byte("a"), int(MaxInboxAttachmentBytes)+1))
	require.Contains(t, rej, "limit")
}

func TestInspectInboxAttachment_ShebangWithExeNameRejected(t *testing.T) {
	// Extension denylist applies before content checks: a script named
	// .exe is still rejected (confusing-by-name is disallowed).
	_, rej := InspectInboxAttachment("evil.exe", []byte("#!/bin/sh\necho hi\n"))
	require.Contains(t, rej, "extension")
}
