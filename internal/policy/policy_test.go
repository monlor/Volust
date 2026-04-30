package policy

import "testing"

func TestParseRetentionAcceptsSupportedKeys(t *testing.T) {
	retention, err := ParseRetention("keep-last=7,keep-daily=7,keep-weekly=4,keep-monthly=6")
	if err != nil {
		t.Fatalf("ParseRetention returned error: %v", err)
	}

	want := []string{"--keep-last", "7", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6"}
	if got := retention.Args(); !equalStrings(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

func TestParseRetentionAcceptsKeepTagValues(t *testing.T) {
	retention, err := ParseRetention("keep-last=7,keep-tag=important")
	if err != nil {
		t.Fatalf("ParseRetention returned error: %v", err)
	}

	want := []string{"--keep-last", "7", "--keep-tag", "important"}
	if got := retention.Args(); !equalStrings(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

func TestParseRetentionRejectsUnknownKey(t *testing.T) {
	if _, err := ParseRetention("keep-hourly=24"); err == nil {
		t.Fatal("ParseRetention succeeded for unsupported key")
	}
}

func TestParseScheduleRequiresFiveFieldCron(t *testing.T) {
	if _, err := ParseSchedule("0 3 * * *"); err != nil {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	if _, err := ParseSchedule("@daily"); err == nil {
		t.Fatal("@daily schedule was accepted")
	}
	if _, err := ParseSchedule("0 0 3 * * *"); err == nil {
		t.Fatal("six-field schedule was accepted")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
