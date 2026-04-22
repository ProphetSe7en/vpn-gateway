package main

import "unicode/utf8"

// sanitizeLogField strips control characters (CR, LF, NUL, other < 0x20)
// and caps length at 1024 bytes on a UTF-8 boundary so downstream log
// readers (ELK, Splunk) never see a partial multi-byte sequence. Prevents
// log forgery: an authenticated caller submitting a field with embedded
// \n must not be able to inject fake log lines downstream. Mirrors the
// Clonarr helper of the same name.
func sanitizeLogField(s string) string {
	const maxLen = 1024
	if len(s) > maxLen {
		s = s[:maxLen]
		// Trim trailing continuation bytes that landed mid-rune. Up to 3
		// bytes possible for a 4-byte UTF-8 sequence.
		for i := 0; i < 3 && len(s) > 0 && !utf8.ValidString(s); i++ {
			s = s[:len(s)-1]
		}
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			b = append(b, ' ')
			continue
		}
		b = append(b, c)
	}
	return string(b)
}
