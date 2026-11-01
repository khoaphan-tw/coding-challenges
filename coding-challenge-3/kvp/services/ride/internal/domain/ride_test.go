package domain

import (
	"errors"
	"testing"
)

func at(s Status) Ride { return Ride{ID: "R1", Status: s} }

func assigned(driver, offer string) Ride {
	return Ride{ID: "R1", Status: DriverAssigned, AssignedDriverID: &driver, CurrentOfferID: &offer}
}

func TestValidTransitions(t *testing.T) {
	cases := []struct {
		from Ride
		act  Action
		to   Status
	}{
		{at(Requested), Action{Kind: StartMatching}, Matching},
		{at(Matching), Action{Kind: AssignDriver, DriverID: "D1", OfferID: "O1"}, DriverAssigned},
		{assigned("D1", "O1"), Action{Kind: MarkAccepted, OfferID: "O1"}, DriverAccepted},
		{assigned("D1", "O1"), Action{Kind: Unassign, OfferID: "O1"}, Matching},
		{at(DriverAccepted), Action{Kind: Complete}, Completed},
		{at(Requested), Action{Kind: Cancel}, Cancelled},
		{at(Matching), Action{Kind: Cancel}, Cancelled},
		{assigned("D1", "O1"), Action{Kind: Cancel}, Cancelled},
		{at(DriverAccepted), Action{Kind: Cancel}, Cancelled},
	}
	for _, c := range cases {
		t.Run(string(c.from.Status)+"->"+string(c.to), func(t *testing.T) {
			next, changed, err := Apply(c.from, c.act)
			if err != nil || !changed || next.Status != c.to {
				t.Fatalf("got %s changed=%v err=%v", next.Status, changed, err)
			}
		})
	}
}

func TestAssignStoresDriver(t *testing.T) {
	next, _, _ := Apply(at(Matching), Action{Kind: AssignDriver, DriverID: "D1", OfferID: "O1"})
	if next.AssignedDriverID == nil || *next.AssignedDriverID != "D1" || *next.CurrentOfferID != "O1" {
		t.Fatalf("driver not stored: %+v", next)
	}
}

func TestRejectionReturnsToMatchingAndClearsDriver(t *testing.T) {
	next, changed, err := Apply(assigned("D1", "O1"), Action{Kind: Unassign, OfferID: "O1"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if next.Status != Matching || next.AssignedDriverID != nil || next.CurrentOfferID != nil {
		t.Fatalf("expected MATCHING with no driver, got %+v", next)
	}
}

func TestInvalidTransitions(t *testing.T) {
	cases := []struct {
		name string
		from Ride
		act  Action
	}{
		{"COMPLETED -> DRIVER_ASSIGNED", at(Completed), Action{Kind: AssignDriver, DriverID: "D1", OfferID: "O1"}},
		{"COMPLETED -> CANCELLED", at(Completed), Action{Kind: Cancel}},
		{"CANCELLED -> COMPLETED", at(Cancelled), Action{Kind: Complete}},
		{"CANCELLED -> MATCHING", at(Cancelled), Action{Kind: StartMatching}},
		{"CANCELLED -> DRIVER_ACCEPTED", at(Cancelled), Action{Kind: MarkAccepted, OfferID: "O1"}},
		{"REQUESTED -> DRIVER_ASSIGNED", at(Requested), Action{Kind: AssignDriver, DriverID: "D1", OfferID: "O1"}},
		{"REQUESTED -> COMPLETED", at(Requested), Action{Kind: Complete}},
		{"MATCHING -> DRIVER_ACCEPTED", at(Matching), Action{Kind: MarkAccepted, OfferID: "O1"}},
		{"DRIVER_ASSIGNED -> COMPLETED", assigned("D1", "O1"), Action{Kind: Complete}},
		{"accept for another offer", assigned("D1", "O1"), Action{Kind: MarkAccepted, OfferID: "O2"}},
		{"assign over another offer", assigned("D1", "O1"), Action{Kind: AssignDriver, DriverID: "D2", OfferID: "O2"}},
		{"DRIVER_ACCEPTED -> MATCHING", at(DriverAccepted), Action{Kind: Unassign, OfferID: "O1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, changed, err := Apply(c.from, c.act)
			if !errors.Is(err, ErrInvalidTransition) || changed || next.Status != c.from.Status {
				t.Fatalf("expected rejection, got %s changed=%v err=%v", next.Status, changed, err)
			}
		})
	}
}

func TestDuplicateTransitionsAreNoOps(t *testing.T) {
	cases := []struct {
		name string
		from Ride
		act  Action
	}{
		{"StartMatching twice", at(Matching), Action{Kind: StartMatching}},
		{"AssignDriver twice", assigned("D1", "O1"), Action{Kind: AssignDriver, DriverID: "D1", OfferID: "O1"}},
		{"Unassign twice", at(Matching), Action{Kind: Unassign, OfferID: "O1"}},
		{"stale Unassign of an older offer", assigned("D2", "O2"), Action{Kind: Unassign, OfferID: "O1"}},
		{"Cancel twice", at(Cancelled), Action{Kind: Cancel}},
		{"Complete twice", at(Completed), Action{Kind: Complete}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, changed, err := Apply(c.from, c.act)
			if err != nil || changed || next.Status != c.from.Status {
				t.Fatalf("expected no-op, got %s changed=%v err=%v", next.Status, changed, err)
			}
		})
	}

	// MarkAccepted twice: second is a no-op.
	r := assigned("D1", "O1")
	r, _, _ = Apply(r, Action{Kind: MarkAccepted, OfferID: "O1"})
	_, changed, err := Apply(r, Action{Kind: MarkAccepted, OfferID: "O1"})
	if err != nil || changed {
		t.Fatalf("duplicate accept: changed=%v err=%v", changed, err)
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []Status{Cancelled, Completed} {
		if !s.Terminal() {
			t.Fatalf("%s must be terminal", s)
		}
		for _, k := range []ActionKind{StartMatching, AssignDriver, MarkAccepted} {
			if _, _, err := Apply(at(s), Action{Kind: k, DriverID: "D1", OfferID: "O1"}); err == nil {
				t.Fatalf("%s accepted %s", s, k)
			}
		}
	}
}
