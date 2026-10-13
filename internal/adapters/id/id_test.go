package id

import (
	"strings"
	"testing"
)

func TestRandomIDs(t *testing.T) {
	g := Random{}
	report := g.NewReportID()
	delivery := g.NewDeliveryID()

	if !strings.HasPrefix(string(report), "rep_") || len(string(report)) != len("rep_")+32 {
		t.Fatalf("report id = %q", report)
	}
	if !strings.HasPrefix(string(delivery), "del_") || len(string(delivery)) != len("del_")+32 {
		t.Fatalf("delivery id = %q", delivery)
	}
	if g.NewReportID() == report {
		t.Fatal("ids must be unique")
	}
}
