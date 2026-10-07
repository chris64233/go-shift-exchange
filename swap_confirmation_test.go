package goshiftexchange

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestTwoWaySwap_FreezesEmployeesShiftsAndScheduleVersion(t *testing.T) {
	f := newFixture(t)
	beforeVersion := f.svc.ScheduleVersion()

	req, err := f.svc.CreateTwoWaySwap("s3", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if req.ScheduleVersion != beforeVersion {
		t.Fatalf("schedule version = %d, want %d", req.ScheduleVersion, beforeVersion)
	}
	if fmt.Sprint(req.Participants) != "[alice carol]" {
		t.Fatalf("participants = %v", req.Participants)
	}
	if len(req.Rotations) != 2 {
		t.Fatalf("rotations = %+v", req.Rotations)
	}
	wantTargets := map[string]string{"s1": "carol", "s3": "alice"}
	for _, rotation := range req.Rotations {
		if rotation.NewEmployeeID != wantTargets[rotation.ShiftID] {
			t.Fatalf("rotation %+v, want target %s", rotation, wantTargets[rotation.ShiftID])
		}
	}
}

func TestTwoWaySwap_StaffingFailureChangesNeitherSide(t *testing.T) {
	svc, _ := NewService(NewMemoryStore(), WithMinRest(0))
	for _, emp := range []struct {
		id  string
		pos []string
	}{
		{"alice", []string{"A", "B"}},
		{"carol", []string{"A", "B"}},
	} {
		if _, err := svc.RegisterEmployee(emp.id, emp.id, emp.pos); err != nil {
			t.Fatal(err)
		}
	}
	mustShift := func(id, position, employee string, day int) {
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: position, EmployeeID: employee,
			Start: dayTime(day, 8, 0), End: dayTime(day, 16, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mustShift("a", "A", "alice", 1)
	mustShift("b", "B", "carol", 1)

	req, err := svc.CreateTwoWaySwap("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStaffingRequirement(StaffingRequirement{
		Position: "A",
		Start:    dayTime(1, 0, 0),
		End:      dayTime(2, 0, 0),
		MinStaff: 2,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.Agree(req.ID, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if result.Request.Status != SwapFailed || result.Request.Finalization == nil || result.Request.Finalization.Completed {
		t.Fatalf("result = %+v, want failed finalization", result.Request)
	}
	assertHolders(t, svc, map[string]string{"a": "alice", "b": "carol"})
	if sh, _ := svc.GetShift("a"); sh.Version != 1 {
		t.Fatalf("shift a version = %d, want 1", sh.Version)
	}
	if sh, _ := svc.GetShift("b"); sh.Version != 1 {
		t.Fatalf("shift b version = %d, want 1", sh.Version)
	}

	repeat, err := svc.Agree(req.ID, "carol")
	if err != nil || !repeat.Idempotent || repeat.Request.Status != SwapFailed {
		t.Fatalf("repeat confirmation = %+v, %v", repeat, err)
	}
}

func TestTwoWaySwap_DuplicateOccupancyConflictAndRelease(t *testing.T) {
	f := newFixture(t)
	first, err := f.svc.CreateTwoWaySwap("s1", "s3")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CreateTwoWaySwap("s3", "s1")
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("duplicate pending shift: want conflict, got %v", err)
	}

	if _, err := f.svc.CancelSwapRequest(first.ID, "admin-1", "duplicate review"); err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.CreateTwoWaySwap("s1", "s3")
	if err != nil {
		t.Fatalf("shift should be releasable after cancellation: %v", err)
	}
	assertHolders(t, f.svc, map[string]string{"s1": "alice", "s3": "carol"})

	if _, err := f.svc.CancelSwapRequest(second.ID, "admin-1", "manual review"); err != nil {
		t.Fatal(err)
	}
	repeatCancel, err := f.svc.CancelSwapRequest(second.ID, "admin-1", "manual review")
	if err != nil || !repeatCancel.Idempotent || repeatCancel.Request.Status != SwapCanceled {
		t.Fatalf("repeat cancel = %+v, %v", repeatCancel, err)
	}
}

func TestTwoWaySwap_ScheduleAdjustmentMakesLateConfirmationConflict(t *testing.T) {
	f := newFixture(t)
	req, err := f.svc.CreateTwoWaySwap("s1", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "extra", Position: "A", EmployeeID: "dave",
		Start: dayTime(9, 8, 0), End: dayTime(9, 16, 0),
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := f.svc.GetSwapRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != SwapFailed {
		t.Fatalf("status = %s, want failed", stored.Status)
	}
	result, err := f.svc.Agree(req.ID, "carol")
	var conflict *ConflictError
	if !errors.As(err, &conflict) || result != nil {
		t.Fatalf("late agree = %+v, %v; want conflict", result, err)
	}
	assertHolders(t, f.svc, map[string]string{"s1": "alice", "s3": "carol"})
}

func TestConcurrent_TwoWayAgreeVsAdminCancel(t *testing.T) {
	for iteration := 0; iteration < 300; iteration++ {
		f := newFixture(t)
		req, err := f.svc.CreateTwoWaySwap("s1", "s3")
		if err != nil {
			t.Fatalf("iteration %d: create: %v", iteration, err)
		}
		if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
			t.Fatalf("iteration %d: agree: %v", iteration, err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var agreeResult *ActionResult
		var agreeErr, cancelErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			agreeResult, agreeErr = f.svc.Agree(req.ID, "carol")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = f.svc.CancelSwapRequest(req.ID, "admin", "manual review")
		}()
		close(start)
		wg.Wait()

		stored, _ := f.svc.GetSwapRequest(req.ID)
		switch stored.Status {
		case SwapCompleted:
			if cancelErr == nil || agreeErr != nil || agreeResult == nil {
				t.Fatalf("iteration %d: completed state agreeErr=%v cancelErr=%v", iteration, agreeErr, cancelErr)
			}
			assertHolders(t, f.svc, map[string]string{"s1": "carol", "s3": "alice"})
		case SwapCanceled:
			if agreeErr == nil || cancelErr != nil {
				t.Fatalf("iteration %d: canceled state agreeResult=%+v agreeErr=%v cancelErr=%v",
					iteration, agreeResult, agreeErr, cancelErr)
			}
			assertHolders(t, f.svc, map[string]string{"s1": "alice", "s3": "carol"})
		default:
			t.Fatalf("iteration %d: unexpected status %s", iteration, stored.Status)
		}
	}
}

func TestTwoWaySwap_CompletedSwapInvalidatesOtherPendingRequest(t *testing.T) {
	f := newFixture(t)
	completed, err := f.svc.CreateTwoWaySwap("s1", "s3")
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.svc.CreateTwoWaySwap("s2", "s4")
	if err != nil {
		t.Fatal(err)
	}
	f.agreeAll(t, completed.ID, "alice", "carol")

	stored, err := f.svc.GetSwapRequest(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != SwapFailed || stored.ScheduleVersion >= f.svc.ScheduleVersion() {
		t.Fatalf("other request = %+v, current version %d", stored, f.svc.ScheduleVersion())
	}
	if h, v := f.holder(t, "s2"); h != "bob" || v != 1 {
		t.Fatalf("s2 = %s v%d, want bob v1", h, v)
	}
	if h, v := f.holder(t, "s4"); h != "dave" || v != 1 {
		t.Fatalf("s4 = %s v%d, want dave v1", h, v)
	}
}

func assertHolders(t *testing.T, svc *Service, want map[string]string) {
	t.Helper()
	for shiftID, holder := range want {
		sh, err := svc.GetShift(shiftID)
		if err != nil {
			t.Fatal(err)
		}
		if sh.EmployeeID != holder {
			t.Fatalf("shift %s holder = %s, want %s", shiftID, sh.EmployeeID, holder)
		}
	}
}
