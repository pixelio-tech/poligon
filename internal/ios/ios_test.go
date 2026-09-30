package ios

import "testing"

func TestOSMajor(t *testing.T) {
	for v, want := range map[string]int{"26.3": 26, "17.0.1": 17, "15.8.5": 15, "": 0, " 18": 18, "beta": 0} {
		if got := osMajor(v); got != want {
			t.Errorf("osMajor(%q) = %d, want %d", v, got, want)
		}
	}
}
