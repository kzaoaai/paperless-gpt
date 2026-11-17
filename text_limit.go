package main

import (
	"strings"
	"unicode"
)

// paperless-ngx keeps a text custom field in 128 characters and refuses a
// longer value, and with it the whole update. The prompt states the limit and
// the model still overruns it now and then, by a few characters. Sending the
// value again does not help, because the next answer is as long, so the value
// is cut to fit before it is sent: the field is filled and the document is not
// marked failed.
const customFieldTextMax = 128

// fitText cuts s to at most max characters, at the last whole word that fits.
// A dangling separator at the cut is dropped. A text that fits is returned
// unchanged.
func fitText(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	cut := max
	if !unicode.IsSpace(r[max]) {
		for cut > 0 && !unicode.IsSpace(r[cut-1]) {
			cut--
		}
	}
	dangling := func(c rune) bool { return unicode.IsSpace(c) || strings.ContainsRune(",;:-–—/(&+", c) }
	if out := strings.TrimRightFunc(string(r[:cut]), dangling); out != "" {
		return out
	}
	// One word longer than the field: there is no whole word to keep.
	return string(r[:max])
}

// fitCustomFieldText returns a text custom field's value cut to what
// paperless-ngx holds. The full text goes to the log, the only place it is
// kept. Other data types and values that fit are returned unchanged.
func fitCustomFieldText(documentID int, field CustomFieldSuggestion, dataType string, value interface{}) interface{} {
	s, ok := value.(string)
	if !ok || dataType != "string" {
		return value
	}
	fitted := fitText(s, customFieldTextMax)
	if fitted == s {
		return value
	}
	log.Warnf("Document %d: custom field %q (id %d) was %d characters and paperless-ngx holds %d, cut to %d at a word. Full text: %q",
		documentID, field.Name, field.ID, len([]rune(s)), customFieldTextMax, len([]rune(fitted)), s)
	return fitted
}
