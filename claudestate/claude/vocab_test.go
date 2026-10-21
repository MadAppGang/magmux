package claude

import "testing"

// The table is non-trivial, every list is duplicate-free, and the homographs
// are words (banned in decode position only, G1).
func TestTheVocabularyIsPopulatedAndConsistent(t *testing.T) {
	v := Vocabulary()
	lists := map[string][]string{
		"Registry.Universal": v.Registry.Universal, "Registry.Optional": v.Registry.Optional,
		"Jobs.Universal": v.Jobs.Universal, "Jobs.Optional": v.Jobs.Optional,
		"AgentsInteractive": v.AgentsInteractive.Universal, "AgentsBackground": v.AgentsBackground.Universal,
		"Timeline": v.Timeline.Universal, "JobsNeverRead": v.JobsNeverRead, "TranscriptKeys": v.TranscriptKeys,
		"RegistryStatus": v.RegistryStatus, "WaitingFor": v.WaitingFor, "JobState": v.JobState, "Tempo": v.Tempo,
		"Kind": v.Kind, "UntimestampedTypes": v.UntimestampedTypes, "Homographs": v.Homographs,
	}
	for name, l := range lists {
		if len(l) == 0 {
			t.Errorf("%s is empty", name)
		}
		seen := map[string]bool{}
		for _, s := range l {
			if s == "" || seen[s] {
				t.Errorf("%s: empty or duplicate %q", name, s)
			}
			seen[s] = true
		}
	}
	if len(v.Registry.Universal) != 19 || len(v.Jobs.Universal) != 19 || len(v.UntimestampedTypes) != 8 {
		t.Fatalf("the measured sets: %d registry, %d jobs, %d untimestamped", len(v.Registry.Universal), len(v.Jobs.Universal), len(v.UntimestampedTypes))
	}
	for _, k := range v.JobsNeverRead {
		found := false
		for _, u := range append(append([]string{}, v.Jobs.Universal...), v.Jobs.Optional...) {
			found = found || u == k
		}
		if !found {
			t.Errorf("never-read key %q is not in the measured union, so jobs-keys would warn on it", k)
		}
	}
	for _, h := range v.Homographs {
		if !isWord(h) {
			t.Errorf("homograph %q is not a word", h)
		}
	}
	v.Registry.Universal[0] = "mutated"
	if Vocabulary().Registry.Universal[0] == "mutated" {
		t.Fatal("Vocabulary returns a shared table")
	}
}
