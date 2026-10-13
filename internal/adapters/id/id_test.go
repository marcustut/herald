package id

import (
	"strings"
	"testing"
)

func TestRandomIDs(t *testing.T) {
	g := Random{}
	event := g.NewEventID()
	delivery := g.NewDeliveryID()

	if !strings.HasPrefix(string(event), "evt_") || len(string(event)) != len("evt_")+32 {
		t.Fatalf("event id = %q", event)
	}
	if !strings.HasPrefix(string(delivery), "del_") || len(string(delivery)) != len("del_")+32 {
		t.Fatalf("delivery id = %q", delivery)
	}
	if g.NewEventID() == event {
		t.Fatal("ids must be unique")
	}
}
