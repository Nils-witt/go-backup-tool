package notify

import (
	"encoding/json"
	"mime"
	"strings"
)

// NoEscape returns s unchanged: the escape function for templates whose
// output isn't parsed by anything, i.e. email subjects and bodies.
func NoEscape(s string) string {
	return s
}

// EscapeJSONString returns s encoded as the inside of a JSON string
// literal — quotes, backslashes and control characters escaped, without the
// surrounding quotes — so a placeholder value substituted into a JSON
// template's string can't end that string and add or override fields.
func EscapeJSONString(s string) string {
	// Marshaling a string never fails.
	b, _ := json.Marshal(s)

	return string(b[1 : len(b)-1])
}

// TemplateEscaper returns how placeholder values must be escaped before
// they're substituted into wh.Body: EscapeJSONString when the request's
// effective Content-Type (wh.Headers' Content-Type, else
// defaultContentType, as PostWebhook sends it) is JSON, since a JSON
// template puts its placeholders inside string literals; NoEscape
// otherwise. Values such as a receiver object key or a remote receiver's
// error text come from other instances, and must not be able to rewrite
// the alert the operator's template describes.
func (wh Webhook) TemplateEscaper(defaultContentType string) func(string) string {
	contentType := defaultContentType

	for k, v := range wh.Headers {
		if strings.EqualFold(k, "Content-Type") {
			contentType = v
		}
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(contentType))
	}

	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return EscapeJSONString
	}

	return NoEscape
}
