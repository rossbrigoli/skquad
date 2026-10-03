package domain

// S-216: inbox attachment content policy.
//
// Agents may deliver files to the human owner's inbox, but only files a
// human can INSPECT: documents, images, video, audio, plain-text and
// shebang scripts. Compiled/executable binaries are rejected at ingest —
// the server must never store or serve a blob that a browser or OS might
// treat as a program.
//
// The policy is deliberately layered so no single bypass (a lying file
// extension, a spoofed Content-Type header, a renamed binary) is
// sufficient:
//
//  1. Extension denylist — obvious executable extensions are refused
//     regardless of content (defense-in-depth for the human's own
//     download habits; a ".exe" that sniffs as text is still confusing
//     and rejected).
//  2. Magic-byte rejection — ELF, PE/Windows, Mach-O (32/64, both
//     endians, fat), WebAssembly and Java class files are refused on
//     their leading bytes no matter what they are named.
//  3. Inspectability allowlist — the sniffed content type must be a
//     known inspectable family (text/*, images, video, audio, PDF,
//     zip-based documents). Anything else that looks binary (NUL bytes
//     in the head window) is rejected as uninspectable.
//
// Text scripts (.sh/.py/.js, shebang or not) are explicitly ALLOWED by
// the card: they are inspectable source, not opaque compiled artifacts.

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// Attachment limits (documented in docs/ and in the send_inbox tool
// description). Kept here so the control-plane handler and tests share
// one source of truth.
const (
	MaxInboxAttachmentBytes   int64 = 25 << 20 // 25 MiB per file
	MaxInboxAttachmentsPerMsg       = 8
)

// executableExtensions are refused by name. Scripts (.sh, .py, .js,
// .ps1, .bat, ...) are intentionally NOT in this list — they are
// inspectable text per the S-216 card.
var executableExtensions = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".ocx": true,
	".sys": true, ".scr": true, ".com": true, ".msi": true, ".msm": true,
	".bin": true, ".elf": true, ".o": true, ".a": true, ".ko": true,
	".wasm": true, ".class": true, ".jar": true, ".dex": true, ".x86": true,
	".arm": true, ".efi": true, ".cpl": true, ".drv": true, ".vxd": true,
}

// executableMagic pairs a human-readable format name with a matcher over
// the leading bytes of a file.
var executableMagic = []struct {
	name  string
	match func(head []byte) bool
}{
	{"ELF", func(h []byte) bool {
		return len(h) >= 4 && h[0] == 0x7f && h[1] == 'E' && h[2] == 'L' && h[3] == 'F'
	}},
	{"PE/DOS executable", func(h []byte) bool {
		// "MZ" at offset 0 is the DOS/PE signature. Legitimate text
		// documents essentially never begin with these two bytes.
		return len(h) >= 2 && h[0] == 'M' && h[1] == 'Z'
	}},
	{"Mach-O binary", func(h []byte) bool {
		if len(h) < 4 {
			return false
		}
		// 0xFEEDFACE (32-bit), 0xFEEDFACF (64-bit) and byte-swapped
		// variants 0xCEFAEDDE / 0xCFFAEDFE.
		return (h[0] == 0xfe && h[1] == 0xed && h[2] == 0xfa && (h[3] == 0xce || h[3] == 0xcf)) ||
			(h[0] == 0xcf && h[1] == 0xfa && h[2] == 0xed && h[3] == 0xfe) ||
			(h[0] == 0xce && h[1] == 0xfa && h[2] == 0xed && h[3] == 0xde)
	}},
	{"WebAssembly", func(h []byte) bool {
		return len(h) >= 4 && h[0] == 0x00 && h[1] == 0x61 && h[2] == 0x73 && h[3] == 0x6d
	}},
	{"Java class / Mach-O fat binary", func(h []byte) bool {
		// 0xCAFEBABE is both the Java .class magic and the Mach-O fat
		// binary magic — both are compiled artifacts.
		return len(h) >= 4 && h[0] == 0xca && h[1] == 0xfe && h[2] == 0xba && h[3] == 0xbe
	}},
}

// inspectableContentTypes are canonical MIME types (as produced by
// sniffContentType) accepted for non-text attachments.
var inspectableContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
	"video/mp4":  true,
	"video/webm": true,
	"video/x-matroska": true,
	"audio/mpeg":       true,
	"audio/ogg":        true,
	"audio/wav":        true,
	"audio/flac":       true,
	"application/pdf":  true,
	"application/zip":  true, // also carries docx/xlsx/pptx/odt documents
}

// InspectInboxAttachment validates one attachment's bytes and returns
// the canonical (server-sniffed) content type. A non-empty second
// return value is a human-readable rejection reason; the caller must
// treat the attachment as rejected.
func InspectInboxAttachment(filename string, data []byte) (contentType string, rejection string) {
	if len(data) == 0 {
		return "", "file is empty"
	}
	if int64(len(data)) > MaxInboxAttachmentBytes {
		return "", fmt.Sprintf("file exceeds the %d MB limit", MaxInboxAttachmentBytes>>20)
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if executableExtensions[ext] {
		return "", fmt.Sprintf("executable file extension %q is not allowed", ext)
	}

	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, m := range executableMagic {
		if m.match(head) {
			return "", fmt.Sprintf("file content is a %s (executable binaries are rejected)", m.name)
		}
	}

	sniffed := sniffContentType(head)
	if strings.HasPrefix(sniffed, "text/") || sniffed == "application/json" || sniffed == "application/xml" {
		return sniffed, ""
	}
	if inspectableContentTypes[sniffed] {
		return sniffed, ""
	}
	// Unknown type: fall back to the NUL-byte heuristic. Text-ish content
	// (even if http.DetectContentType mislabelled it) passes; anything
	// with binary NULs in the head window is uninspectable and rejected.
	if bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Sprintf("uninspectable binary content (detected %s); only documents, images, video, audio and text files are accepted", sniffed)
	}
	return "text/plain", ""
}

// sniffContentType returns a canonical MIME type for the head bytes,
// extending http.DetectContentType with the formats it does not know.
func sniffContentType(head []byte) string {
	// http.DetectContentType wants at least 512 bytes; pad defensively.
	padded := head
	if len(padded) < 512 {
		padded = append(append([]byte{}, padded...), bytes.Repeat([]byte{' '}, 512-len(padded))...)
	}
	detected := http.DetectContentType(padded)
	switch detected {
	case "text/plain; charset=utf-8":
		return "text/plain"
	case "application/ogg":
		// DetectContentType knows the container but not that we treat
		// Ogg as inspectable audio.
		return "audio/ogg"
	case "application/octet-stream":
		// Custom sniffs for common binaries DetectContentType misses.
		switch {
		case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
			return "image/webp"
		case len(head) >= 12 && string(head[4:8]) == "ftyp":
			return "video/mp4"
		case len(head) >= 4 && head[0] == 0x1a && head[1] == 0x45 && head[2] == 0xdf && head[3] == 0xa3:
			return "video/webm" // EBML (webm/matroska)
		case len(head) >= 4 && string(head[0:4]) == "OggS":
			return "audio/ogg"
		case len(head) >= 4 && string(head[0:4]) == "fLaC":
			return "audio/flac"
		}
	}
	return detected
}
