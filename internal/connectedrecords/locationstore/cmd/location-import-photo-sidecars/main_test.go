package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSidecarsWithNoLocationOrRepeatedAreLeftOut(t *testing.T) {
	directory := t.TempDir()
	write := func(batch, content string) {
		if err := os.MkdirAll(filepath.Join(directory, batch), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, batch, SidecarFileName), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("batch-1", `{"sha256":"AA","geoData":{"latitude":10.1,"longitude":20.1,"altitude":0}}
{"sha256":"bb","geoData":{"latitude":0.0,"longitude":0.0}}
`)
	write("batch-2", `{"sha256":"aa","geoData":{"latitude":10.2,"longitude":20.2}}
{"sha256":"cc","geoData":{"latitude":10.3,"longitude":20.3}}
`)
	report := map[string]int{}
	located, err := readSidecars(directory, report)
	if err != nil {
		t.Fatal(err)
	}
	if len(located) != 2 || located[0].Sha256 != "aa" || located[0].GeoData.Latitude != 10.1 || located[1].Sha256 != "cc" {
		t.Errorf("located = %+v, want aa (the first sidecar's place) and cc", located)
	}
	if report["sidecars with no geoData"] != 1 || len(report) != 2 {
		t.Errorf("report = %v, want one with no geoData and one placed elsewhere", report)
	}
	if _, err := readSidecars(t.TempDir(), map[string]int{}); err == nil {
		t.Error("a directory with no sidecars was read without complaint")
	}
}
