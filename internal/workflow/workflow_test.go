package workflow

import "testing"

func TestWakeEventIsStableAndJobScoped(t *testing.T) {
	if WakeEvent("job-a") != WakeEvent("job-a") {
		t.Fatal("same Job did not retain its wake identity")
	}
	if WakeEvent("job-a") == WakeEvent("job-b") {
		t.Fatal("distinct Jobs share a wake event")
	}
}
