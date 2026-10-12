package notify

import (
	"encoding/json"
	"testing"
)

func TestEscapeJSONString(t *testing.T) {
	t.Parallel()

	for _, in := range []string{`plain`, `a"b`, `back\slash`, "line\nbreak", `x","y":"z`} {
		var got string
		if err := json.Unmarshal([]byte(`"`+EscapeJSONString(in)+`"`), &got); err != nil || got != in {
			t.Errorf("EscapeJSONString(%q) round-trip = (%q, %v), want the input back", in, got, err)
		}
	}
}

func TestWebhookTemplateEscaper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		headers        map[string]string
		defaultCT      string
		expectEscaping bool
	}{
		{name: "json default", defaultCT: "application/json", expectEscaping: true},
		{name: "json with charset header", headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, defaultCT: "text/plain", expectEscaping: true},
		{name: "vendor json suffix", headers: map[string]string{"content-type": "application/vnd.api+json"}, expectEscaping: true},
		{name: "plain text header overrides json default", headers: map[string]string{"CONTENT-TYPE": "text/plain"}, defaultCT: "application/json"},
		{name: "form encoded", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			escape := Webhook{Headers: tt.headers}.TemplateEscaper(tt.defaultCT)
			if escaped := escape(`a"b`) != `a"b`; escaped != tt.expectEscaping {
				t.Errorf("TemplateEscaper() escapes = %v, want %v", escaped, tt.expectEscaping)
			}
		})
	}
}
