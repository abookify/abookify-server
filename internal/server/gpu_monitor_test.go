package server

import "testing"

// deviceTransition is the decision core of the mid-run whisper-device monitor.
// It must: establish a baseline silently on a healthy first read, raise the LOUD
// downgrade alarm ONLY on cuda→cpu after a known-good state, stay quiet while
// stuck on cpu (no re-alarm every tick), re-arm on cpu→cuda recovery, and treat
// an empty read (undecodable /health) as no-signal without disturbing baseline.
func TestDeviceTransition(t *testing.T) {
	cases := []struct {
		name         string
		baseline     string
		dev          string
		wantBaseline string
		wantAlarm    deviceAlarm
	}{
		{"first read cuda → baseline, silent", "", "cuda", "cuda", alarmNone},
		{"first read cpu → baseline, info", "", "cpu", "cpu", alarmStartedCPU},
		{"cuda holds → silent", "cuda", "cuda", "cuda", alarmNone},
		{"cuda→cpu → LOUD downgrade + adopt cpu", "cuda", "cpu", "cpu", alarmDowngrade},
		{"stuck on cpu → no re-alarm", "cpu", "cpu", "cpu", alarmNone},
		{"cpu→cuda recovery → restored", "cpu", "cuda", "cuda", alarmRestored},
		{"empty read from cuda → no-signal, baseline held", "cuda", "", "cuda", alarmNone},
		{"empty read before baseline → still no baseline", "", "", "", alarmNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotBaseline, gotAlarm := deviceTransition(c.baseline, c.dev)
			if gotBaseline != c.wantBaseline {
				t.Errorf("baseline = %q, want %q", gotBaseline, c.wantBaseline)
			}
			if gotAlarm != c.wantAlarm {
				t.Errorf("alarm = %d, want %d", gotAlarm, c.wantAlarm)
			}
		})
	}
}

// A healthy compose recreate (the common case with the COMPOSE_FILE guard armed)
// looks like: cuda baseline → whisper briefly unreachable (loop `continue`s, so
// deviceTransition never sees it) → cuda again. Simulate the reads the function
// DOES see and assert NO false downgrade alarm fires across the blip.
func TestDeviceTransition_HealthyRecreateNoFalseAlarm(t *testing.T) {
	baseline := "cuda"
	// Unreachable ticks are skipped by the loop, so the next read the function
	// sees after the recreate is cuda again.
	for _, dev := range []string{"cuda", "cuda"} {
		var alarm deviceAlarm
		baseline, alarm = deviceTransition(baseline, dev)
		if alarm != alarmNone {
			t.Fatalf("healthy recreate raised a false alarm %d", alarm)
		}
	}
	if baseline != "cuda" {
		t.Fatalf("baseline drifted to %q after a healthy recreate", baseline)
	}
}
