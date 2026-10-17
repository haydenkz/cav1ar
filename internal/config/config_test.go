package config

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/haydenkz/cav1ar/internal/encode"
)

// Settings saved when there was one encoder for everything apply it to
// devices of its kind, and choosing the CPU keeps the GPUs out.
func TestOldEncoderSettingsCarryOver(t *testing.T) {
	for _, c := range []struct {
		saved string
		want  map[string]int
	}{
		{`{"encoder": "nvidia", "workers": 1}`, map[string]int{"nvidia": 1}},
		{`{"encoder": "cpu", "workers": 2}`, map[string]int{"cpu": 2, "nvidia": 0, "vaapi": 0}},
		{`{"encoder": "nvidia", "workers": 1, "encoders": {"cpu": 0}}`, map[string]int{"cpu": 0}}, // already new
		{`{"crf": 30}`, nil},
	} {
		s := encode.DefaultSettings()
		if err := json.Unmarshal([]byte(c.saved), &s); err != nil {
			t.Fatal(err)
		}
		migrate(&s, []byte(c.saved))
		if !maps.Equal(s.Encoders, c.want) {
			t.Errorf("%s: got %v, want %v", c.saved, s.Encoders, c.want)
		}
	}
}
