package domain

import (
	"errors"
	"testing"
)

func at(s Status, ride string) Driver {
	d := Driver{ID: "D1", Status: s}
	if ride != "" {
		d.CurrentRideID = &ride
	}
	return d
}

func TestAvailabilityToggle(t *testing.T) {
	d, changed, err := SetAvailability(at(Offline, ""), Available)
	if err != nil || !changed || d.Status != Available {
		t.Fatalf("OFFLINE->AVAILABLE: %v %v %v", d.Status, changed, err)
	}
	d, changed, err = SetAvailability(d, Offline)
	if err != nil || !changed || d.Status != Offline {
		t.Fatalf("AVAILABLE->OFFLINE: %v %v %v", d.Status, changed, err)
	}
	if _, changed, err = SetAvailability(d, Offline); err != nil || changed {
		t.Fatalf("duplicate OFFLINE must be a no-op: %v %v", changed, err)
	}
}

func TestOfferedOrBusyCannotGoOffline(t *testing.T) {
	for _, s := range []Status{Offered, Busy} {
		for _, target := range []Status{Offline, Available} {
			if _, _, err := SetAvailability(at(s, "R1"), target); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("%s -> %s via availability API must be rejected, got %v", s, target, err)
			}
		}
	}
	if _, _, err := SetAvailability(at(Available, ""), Busy); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("availability API cannot set BUSY")
	}
}

func TestOfferMakesAvailableOffered(t *testing.T) {
	d, changed, err := Reserve(at(Available, ""), "R1")
	if err != nil || !changed || d.Status != Offered || *d.CurrentRideID != "R1" {
		t.Fatalf("got %+v %v %v", d, changed, err)
	}
	if _, changed, err := Reserve(d, "R1"); err != nil || changed {
		t.Fatalf("duplicate reserve for the same ride must be a no-op: %v %v", changed, err)
	}
}

func TestOnlyAvailableDriversCanBeOffered(t *testing.T) {
	cases := []Driver{at(Offline, ""), at(Offered, "R1"), at(Busy, "R1")}
	for _, d := range cases {
		if _, _, err := Reserve(d, "R2"); !errors.Is(err, ErrNotAvailable) {
			t.Fatalf("%s driver received an offer for another ride", d.Status)
		}
		if CanReceiveOffer(d) {
			t.Fatalf("%s must not receive offers", d.Status)
		}
	}
}

func TestRejectOrCancelBeforeAcceptReturnsToAvailable(t *testing.T) {
	d, changed, err := Release(at(Offered, "R1"), "R1")
	if err != nil || !changed || d.Status != Available || d.CurrentRideID != nil {
		t.Fatalf("got %+v %v %v", d, changed, err)
	}
}

func TestAcceptMakesBusyAndCompleteReturnsAvailable(t *testing.T) {
	d, changed, err := MarkBusy(at(Offered, "R1"), "R1")
	if err != nil || !changed || d.Status != Busy {
		t.Fatalf("accept: %+v %v %v", d, changed, err)
	}
	if _, changed, err := MarkBusy(d, "R1"); err != nil || changed {
		t.Fatalf("duplicate accept must be a no-op: %v %v", changed, err)
	}
	d, changed, err = Release(d, "R1")
	if err != nil || !changed || d.Status != Available {
		t.Fatalf("complete: %+v %v %v", d, changed, err)
	}
}

func TestInvalidDriverTransitions(t *testing.T) {
	if _, _, err := MarkBusy(at(Available, ""), "R1"); !errors.Is(err, ErrStale) {
		t.Fatal("AVAILABLE -> BUSY must be rejected")
	}
	if _, _, err := MarkBusy(at(Offline, ""), "R1"); !errors.Is(err, ErrStale) {
		t.Fatal("OFFLINE -> BUSY must be rejected")
	}
	if _, _, err := MarkBusy(at(Offered, "R1"), "R2"); !errors.Is(err, ErrStale) {
		t.Fatal("accepting another ride's offer must be rejected")
	}
	if _, _, err := Reserve(at(Offline, ""), "R1"); !errors.Is(err, ErrNotAvailable) {
		t.Fatal("OFFLINE -> OFFERED must be rejected")
	}
}

func TestReleaseForAnotherRideIsIgnored(t *testing.T) {
	d, changed, err := Release(at(Busy, "R2"), "R1")
	if err != nil || changed || d.Status != Busy {
		t.Fatalf("stale release must not free the driver: %+v %v %v", d, changed, err)
	}
}
