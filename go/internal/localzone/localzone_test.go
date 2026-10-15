package localzone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata" // the binary embeds it too, so the zone names resolve as they do on a box
)

// host fakes the two files the resolver reads. Either may be left out.
func host(t *testing.T, linkedZone, timezoneFile string) (localtime, timezone string) {
	t.Helper()
	dir := t.TempDir()
	localtime = filepath.Join(dir, "localtime")
	timezone = filepath.Join(dir, "timezone")
	if linkedZone != "" {
		target := filepath.Join(dir, "usr", "share", "zoneinfo", filepath.FromSlash(linkedZone))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, localtime); err != nil {
			t.Fatal(err)
		}
	}
	if timezoneFile != "" {
		if err := os.WriteFile(timezone, []byte(timezoneFile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return localtime, timezone
}

func TestAHostWithoutTZIsNamedByItsLocaltimeLink(t *testing.T) {
	localtime, timezone := host(t, "Europe/Stockholm", "")
	if got := resolve("", "Local", localtime, timezone); got != "Europe/Stockholm" {
		t.Fatalf("zone = %q, want Europe/Stockholm", got)
	}
}

func TestTZNamesTheZoneBeforeTheHostFiles(t *testing.T) {
	localtime, timezone := host(t, "Europe/Stockholm", "Europe/Oslo\n")
	if got := resolve(":Europe/Berlin", "Europe/Berlin", localtime, timezone); got != "Europe/Berlin" {
		t.Fatalf("zone = %q, want Europe/Berlin", got)
	}
}

func TestAnUnknownTZFallsThroughToTheHost(t *testing.T) {
	localtime, timezone := host(t, "Europe/Stockholm", "")
	if got := resolve("Mars/Olympus", "Mars/Olympus", localtime, timezone); got != "Europe/Stockholm" {
		t.Fatalf("zone = %q, want Europe/Stockholm", got)
	}
}

func TestEtcTimezoneNamesTheZoneWhenLocaltimeIsNotALink(t *testing.T) {
	localtime, timezone := host(t, "", "Europe/Oslo\n")
	if got := resolve("", "Local", localtime, timezone); got != "Europe/Oslo" {
		t.Fatalf("zone = %q, want Europe/Oslo", got)
	}
}

func TestAHostThatNamesNoZoneIsUTC(t *testing.T) {
	localtime, timezone := host(t, "", "")
	if got := resolve("", "Local", localtime, timezone); got != "UTC" {
		t.Fatalf("zone = %q, want UTC", got)
	}
}

func TestNameIsAZoneClientsCanLoad(t *testing.T) {
	got := Name()
	if got == "Local" || got == "" {
		t.Fatalf("Name() = %q, want an IANA name or UTC", got)
	}
	if _, err := time.LoadLocation(got); err != nil {
		t.Fatalf("Name() = %q does not load: %v", got, err)
	}
}
