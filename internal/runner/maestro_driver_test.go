package runner

import (
	"path/filepath"
	"testing"
)

func TestDriverCacheSurvivesRestartAndUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".maestro-drivers.json")
	c := newDriverCache(path)
	if c.current("S1", "2.10.0") {
		t.Fatal("unknown phone must not be trusted")
	}
	c.mark("S1", "2.10.0")

	// a restarted poligon reads what the last one learned
	c2 := newDriverCache(path)
	if !c2.current("S1", "2.10.0") {
		t.Fatal("driver record lost across restart")
	}
	if c2.current("S1", "2.11.0") {
		t.Fatal("a driver from another Maestro version must not be trusted")
	}
	if c2.current("S1", "") {
		t.Fatal("an unreadable Maestro version must not be trusted")
	}
	c2.forget("S1")
	if newDriverCache(path).current("S1", "2.10.0") {
		t.Fatal("forget did not stick")
	}
}
