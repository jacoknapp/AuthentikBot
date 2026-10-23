package main

import (
	"strings"
	"unicode"
)

// recoveryIdentifierFold handles full case-fold expansions as well as the
// standard library's simple Unicode equivalence cycles. It deliberately has
// no locale-dependent mutable state or runtime/network data dependency.
func recoveryIdentifierFold(s string) string {
	var out strings.Builder
	for _, r := range s {
		expansion, ok := recoveryFoldExpansions[r]
		if !ok {
			expansion = string(r)
		}
		for _, v := range expansion {
			least := v
			for next := unicode.SimpleFold(v); next != v; next = unicode.SimpleFold(next) {
				if next < least {
					least = next
				}
			}
			out.WriteRune(least)
		}
	}
	return out.String()
}

// Generated from Python's Unicode 16.0.0 full casefold() multi-rune
// mappings. Single-rune equivalences use Go's unicode.SimpleFold tables.
// Keep these pinned data reviewable; do not download Unicode data at runtime.
var recoveryFoldExpansions = map[rune]string{
	0x00df: "ss",
	0x0130: "i̇",
	0x0149: "ʼn",
	0x01f0: "ǰ",
	0x0390: "ΐ",
	0x03b0: "ΰ",
	0x0587: "եւ",
	0x1e96: "ẖ",
	0x1e97: "ẗ",
	0x1e98: "ẘ",
	0x1e99: "ẙ",
	0x1e9a: "aʾ",
	0x1e9e: "ss",
	0x1f50: "ὐ",
	0x1f52: "ὒ",
	0x1f54: "ὔ",
	0x1f56: "ὖ",
	0x1f80: "ἀι",
	0x1f81: "ἁι",
	0x1f82: "ἂι",
	0x1f83: "ἃι",
	0x1f84: "ἄι",
	0x1f85: "ἅι",
	0x1f86: "ἆι",
	0x1f87: "ἇι",
	0x1f88: "ἀι",
	0x1f89: "ἁι",
	0x1f8a: "ἂι",
	0x1f8b: "ἃι",
	0x1f8c: "ἄι",
	0x1f8d: "ἅι",
	0x1f8e: "ἆι",
	0x1f8f: "ἇι",
	0x1f90: "ἠι",
	0x1f91: "ἡι",
	0x1f92: "ἢι",
	0x1f93: "ἣι",
	0x1f94: "ἤι",
	0x1f95: "ἥι",
	0x1f96: "ἦι",
	0x1f97: "ἧι",
	0x1f98: "ἠι",
	0x1f99: "ἡι",
	0x1f9a: "ἢι",
	0x1f9b: "ἣι",
	0x1f9c: "ἤι",
	0x1f9d: "ἥι",
	0x1f9e: "ἦι",
	0x1f9f: "ἧι",
	0x1fa0: "ὠι",
	0x1fa1: "ὡι",
	0x1fa2: "ὢι",
	0x1fa3: "ὣι",
	0x1fa4: "ὤι",
	0x1fa5: "ὥι",
	0x1fa6: "ὦι",
	0x1fa7: "ὧι",
	0x1fa8: "ὠι",
	0x1fa9: "ὡι",
	0x1faa: "ὢι",
	0x1fab: "ὣι",
	0x1fac: "ὤι",
	0x1fad: "ὥι",
	0x1fae: "ὦι",
	0x1faf: "ὧι",
	0x1fb2: "ὰι",
	0x1fb3: "αι",
	0x1fb4: "άι",
	0x1fb6: "ᾶ",
	0x1fb7: "ᾶι",
	0x1fbc: "αι",
	0x1fc2: "ὴι",
	0x1fc3: "ηι",
	0x1fc4: "ήι",
	0x1fc6: "ῆ",
	0x1fc7: "ῆι",
	0x1fcc: "ηι",
	0x1fd2: "ῒ",
	0x1fd3: "ΐ",
	0x1fd6: "ῖ",
	0x1fd7: "ῗ",
	0x1fe2: "ῢ",
	0x1fe3: "ΰ",
	0x1fe4: "ῤ",
	0x1fe6: "ῦ",
	0x1fe7: "ῧ",
	0x1ff2: "ὼι",
	0x1ff3: "ωι",
	0x1ff4: "ώι",
	0x1ff6: "ῶ",
	0x1ff7: "ῶι",
	0x1ffc: "ωι",
	0xfb00: "ff",
	0xfb01: "fi",
	0xfb02: "fl",
	0xfb03: "ffi",
	0xfb04: "ffl",
	0xfb05: "st",
	0xfb06: "st",
	0xfb13: "մն",
	0xfb14: "մե",
	0xfb15: "մի",
	0xfb16: "վն",
	0xfb17: "մխ",
}
