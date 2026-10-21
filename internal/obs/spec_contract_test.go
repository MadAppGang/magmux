package obs_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

type specEnumCase struct {
	name, wire string
	value      any
	fresh      func() any
}

func specEnums() []specEnumCase {
	s := func() any { v := obs.StatusBusy; return &v }
	w := func() any { v := obs.WaitingReasonInput; return &v }
	l := func() any { v := obs.LivenessYes; return &v }
	return []specEnumCase{
		{"status_unknown", "null", obs.Status(0), s}, {"idle", `"idle"`, obs.StatusIdle, s},
		{"busy", `"busy"`, obs.StatusBusy, s}, {"shell", `"shell"`, obs.StatusShell, s}, {"waiting", `"waiting"`, obs.StatusWaiting, s},
		{"waiting_unknown", "null", obs.WaitingReason(0), w}, {"none", `"none"`, obs.WaitingReasonNone, w},
		{"input", `"input"`, obs.WaitingReasonInput, w}, {"permission", `"permission"`, obs.WaitingReasonPermission, w},
		{"live_unknown", "null", obs.Liveness(0), l}, {"yes", `"yes"`, obs.LivenessYes, l}, {"no", `"no"`, obs.LivenessNo, l},
	}
}
func specJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// WRONG: an enum emits Go identifiers, ordinal numbers, or a negative for unknown.
func TestSpecEnumMarshal(t *testing.T) {
	for _, tt := range specEnums() {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(specJSON(t, tt.value)); got != tt.wire {
				t.Fatalf("got %s, want %s", got, tt.wire)
			}
		})
	}
}

// WRONG: a wire word maps to a different enum, or null leaves an old value standing.
func TestSpecEnumUnmarshal(t *testing.T) {
	for _, tt := range specEnums() {
		t.Run(tt.name, func(t *testing.T) {
			dst := tt.fresh()
			if e := json.Unmarshal([]byte(tt.wire), dst); e != nil {
				t.Fatal(e)
			}
			if got := reflect.ValueOf(dst).Elem().Interface(); !reflect.DeepEqual(got, tt.value) {
				t.Fatalf("got %v, want %v", got, tt.value)
			}
		})
	}
}

// WRONG: the strict core guesses unknown for foreign words or accepts bool/numeric enums.
func TestSpecEnumRejectsForeignJSON(t *testing.T) {
	for _, kind := range []struct {
		name  string
		fresh func() any
	}{{"status", func() any { return new(obs.Status) }}, {"waiting", func() any { return new(obs.WaitingReason) }}, {"live", func() any { return new(obs.Liveness) }}} {
		for _, wire := range []string{`"unknown"`, `""`, `"compacting"`, `"BUSY"`, `"input needed"`, `"permission prompt"`, `"true"`, `0`, `1`, `true`, `false`, `{}`, `[]`} {
			t.Run(kind.name+"/"+wire, func(t *testing.T) {
				if e := json.Unmarshal([]byte(wire), kind.fresh()); e == nil {
					t.Fatalf("accepted %s", wire)
				}
			})
		}
	}
}

// WRONG: an out-of-range ordinal manufactures a valid-looking core document.
func TestSpecEnumRejectsInvalidOrdinal(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    any
	}{{"status", obs.Status(255)}, {"waiting", obs.WaitingReason(255)}, {"live", obs.Liveness(255)}} {
		t.Run(tt.name, func(t *testing.T) {
			if b, e := json.Marshal(tt.v); e == nil {
				t.Fatalf("invalid ordinal encoded as %s", b)
			}
		})
	}
}

// WRONG: omitempty drops needed, or nil is silently emitted as false.
func TestSpecAttentionWire(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    *bool
		want string
	}{{"unknown", nil, `{"needed":null}`}, {"no", obs.Ptr(false), `{"needed":false}`}, {"yes", obs.Ptr(true), `{"needed":true}`}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(specJSON(t, obs.Attention{Needed: tt.v})); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// WRONG: decoding or encoding collapses nil and a determined false.
func TestSpecAttentionRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    *bool
	}{{"unknown", nil}, {"no", obs.Ptr(false)}, {"yes", obs.Ptr(true)}} {
		t.Run(tt.name, func(t *testing.T) {
			want := obs.Attention{Needed: tt.v}
			got := obs.Attention{Needed: obs.Ptr(true)}
			if e := json.Unmarshal(specJSON(t, want), &got); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		})
	}
}

// WRONG: optional zero/empty values disappear or undetermined fields become determined zeros.
func TestSpecOptionalWire(t *testing.T) {
	for _, tt := range []struct {
		name      string
		s         obs.Session
		key, want string
	}{
		{"pane_unknown", obs.Session{}, "paneId", "null"}, {"pane_zero", obs.Session{PaneID: obs.Ptr(0)}, "paneId", "0"},
		{"count_unknown", obs.Session{}, "subagentCount", "null"}, {"count_zero", obs.Session{SubagentCount: obs.Ptr(0)}, "subagentCount", "0"},
		{"color_unknown", obs.Session{}, "color", "null"}, {"color_empty", obs.Session{Color: obs.Ptr("")}, "color", `""`},
		{"since_unknown", obs.Session{}, "statusSince", "null"},
		{"fault_unknown", obs.Session{}, "fault", `{"apiErrors":null}`}, {"fault_zero", obs.Session{Fault: obs.Fault{APIErrors: obs.Ptr(0)}}, "fault", `{"apiErrors":0}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var doc map[string]json.RawMessage
			if e := json.Unmarshal(specJSON(t, tt.s), &doc); e != nil {
				t.Fatal(e)
			}
			if got := string(doc[tt.key]); got != tt.want {
				t.Fatalf("%s=%s, want %s", tt.key, got, tt.want)
			}
		})
	}
}

// WRONG: recording a fault replaces status with error/unknown or drops the fault.
func TestSpecFaultCoexistsWithStatus(t *testing.T) {
	for _, status := range []obs.Status{obs.StatusUnknown, obs.StatusIdle, obs.StatusBusy, obs.StatusShell, obs.StatusWaiting} {
		t.Run(status.String(), func(t *testing.T) {
			want := obs.Session{Status: status, Fault: obs.Fault{APIErrors: obs.Ptr(3), Detail: "quota"}}
			var got obs.Session
			if e := json.Unmarshal(specJSON(t, want), &got); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip lost independent facts: %#v", got)
			}
		})
	}
}

// WRONG: an unknown enum causes its raw diagnostic value to be dropped or normalized.
func TestSpecRawValuesRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    obs.Session
	}{{"status", obs.Session{StatusRaw: " CompActing "}}, {"waiting", obs.Session{WaitingRaw: "future reason"}}} {
		t.Run(tt.name, func(t *testing.T) {
			var got obs.Session
			if e := json.Unmarshal(specJSON(t, tt.s), &got); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(got, tt.s) {
				t.Fatalf("raw value lost: %#v", got)
			}
		})
	}
}

// WRONG: duration is cached, uses the wall clock, treats nil as zero, or clamps clock skew.
func TestSpecInStatusFor(t *testing.T) {
	now := time.Unix(1789545200, 0)
	for _, tt := range []struct {
		name  string
		since *time.Time
		want  time.Duration
		known bool
	}{
		{"unknown", nil, 0, false}, {"real_idle", obs.Ptr(time.Unix(1789536692, 0)), 8508 * time.Second, true},
		{"equal", &now, 0, true}, {"future", obs.Ptr(now.Add(time.Second)), -time.Second, true},
		{"fractional", obs.Ptr(now.Add(-1500 * time.Millisecond)), 1500 * time.Millisecond, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := obs.Session{StatusSince: tt.since}
			got, ok := s.InStatusFor(now)
			if ok != tt.known || (ok && got != tt.want) {
				t.Fatalf("got (%v,%v), want (%v,%v)", got, ok, tt.want, tt.known)
			}
		})
	}
}
func specPopulated() obs.Session {
	return obs.Session{ID: "s", PID: 7, PreviousNames: []string{"first", "second"}, PaneID: obs.Ptr(1), StatusSince: obs.Ptr(time.Unix(1789545200, 0)), SubagentCount: obs.Ptr(2), Color: obs.Ptr("blue"), Attention: obs.Attention{Needed: obs.Ptr(false)}, Fault: obs.Fault{APIErrors: obs.Ptr(3)}}
}

// WRONG: Clone shares any reference (including nested pointers or Fleet's session slice).
func TestSpecCloneIsolation(t *testing.T) {
	mutations := []struct {
		name   string
		change func(*obs.Session)
	}{
		{"names", func(s *obs.Session) { s.PreviousNames[0] = "changed" }}, {"pane", func(s *obs.Session) { *s.PaneID = 99 }},
		{"time", func(s *obs.Session) { *s.StatusSince = time.Time{} }}, {"count", func(s *obs.Session) { *s.SubagentCount = 99 }},
		{"color", func(s *obs.Session) { *s.Color = "red" }}, {"attention", func(s *obs.Session) { *s.Attention.Needed = true }},
		{"fault", func(s *obs.Session) { *s.Fault.APIErrors = 99 }}, {"value", func(s *obs.Session) { s.ID = "changed" }},
	}
	for _, scope := range []string{"session", "fleet"} {
		for _, tt := range mutations {
			t.Run(scope+"/"+tt.name, func(t *testing.T) {
				original := specPopulated()
				want := specPopulated()
				if scope == "session" {
					copy := original.Clone()
					if !reflect.DeepEqual(copy, want) {
						t.Fatal("clone changed values")
					}
					tt.change(&copy)
				} else {
					f := obs.Fleet{Sessions: []obs.Session{original}}
					copy := f.Clone()
					if !reflect.DeepEqual(copy, f) {
						t.Fatal("clone changed values")
					}
					tt.change(&copy.Sessions[0])
					original = f.Sessions[0]
				}
				if !reflect.DeepEqual(original, want) {
					t.Fatalf("mutation leaked into original: %#v", original)
				}
			})
		}
	}
}

// WRONG: Sort uses ID before PID, ignores ties, or drops records.
func TestSpecFleetSort(t *testing.T) {
	for _, tt := range []struct {
		name     string
		in, want []obs.Session
	}{
		{"pid_then_id", []obs.Session{{PID: 2, ID: "a"}, {PID: 1, ID: "z"}, {PID: 1, ID: "b"}}, []obs.Session{{PID: 1, ID: "b"}, {PID: 1, ID: "z"}, {PID: 2, ID: "a"}}},
		{"nil", nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := obs.Fleet{Sessions: tt.in}
			f.Sort()
			if !reflect.DeepEqual(f.Sessions, tt.want) {
				t.Fatalf("got %v, want %v", f.Sessions, tt.want)
			}
		})
	}
}

// WRONG: Fleet omits or replaces the supplied snapshot epoch with a current timestamp.
func TestSpecFleetEpoch(t *testing.T) {
	for _, epoch := range []time.Time{time.Unix(1789545200, 0).UTC(), time.Unix(1789545200, 123456789).UTC()} {
		t.Run(epoch.Format(time.RFC3339Nano), func(t *testing.T) {
			want := obs.Fleet{Epoch: epoch}
			var got obs.Fleet
			if e := json.Unmarshal(specJSON(t, want), &got); e != nil {
				t.Fatal(e)
			}
			if !got.Epoch.Equal(epoch) {
				t.Fatalf("epoch=%v, want %v", got.Epoch, epoch)
			}
		})
	}
}

// WRONG: false converts to undetermined instead of the determined negative.
func TestSpecLivenessOf(t *testing.T) {
	for _, tt := range []struct {
		in   bool
		want obs.Liveness
	}{{false, obs.LivenessNo}, {true, obs.LivenessYes}} {
		t.Run(tt.want.String(), func(t *testing.T) {
			if got := obs.LivenessOf(tt.in); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
