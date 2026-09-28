package goshiftexchange

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

func at(day, hour int) time.Time {
	return base.AddDate(0, 0, day).Add(time.Duration(hour) * time.Hour)
}

func newService(t *testing.T) *Service {
	t.Helper()
	store, err := OpenStore("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return NewService(store, 8*time.Hour)
}

func mustEmployee(t *testing.T, s *Service, id string, quals ...string) {
	t.Helper()
	if err := s.RegisterEmployee(Employee{ID: id, Name: id, Qualifications: quals}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func mustShift(t *testing.T, s *Service, id, empID, qual string, start, end time.Time) Shift {
	t.Helper()
	sh, err := s.ScheduleShift(Shift{
		ID:                    id,
		Position:              qual,
		RequiredQualification: qual,
		Start:                 start,
		End:                   end,
		EmployeeID:            empID,
	})
	if err != nil {
		t.Fatalf("schedule %s: %v", id, err)
	}
	return sh
}

// setupRotation 建立 A、B 两员工各持一个班次的基础场景。
func setupRotation(t *testing.T, s *Service) (shA, shB Shift) {
	t.Helper()
	mustEmployee(t, s, "A", "nurse")
	mustEmployee(t, s, "B", "nurse")
	shA = mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))
	shB = mustShift(t, s, "s2", "B", "nurse", at(2, 8), at(2, 16))
	return shA, shB
}

func TestScheduleShiftValidatesFullSchedule(t *testing.T) {
	s := newService(t)
	mustEmployee(t, s, "A", "nurse")
	mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))

	// 资质不足
	if _, err := s.ScheduleShift(Shift{ID: "s2", RequiredQualification: "doctor",
		Start: at(2, 8), End: at(2, 16), EmployeeID: "A"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected qualification error, got %v", err)
	}
	// 时间重叠
	if _, err := s.ScheduleShift(Shift{ID: "s3", RequiredQualification: "nurse",
		Start: at(1, 15), End: at(1, 23), EmployeeID: "A"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected overlap error, got %v", err)
	}
	// 休息不足 8 小时
	if _, err := s.ScheduleShift(Shift{ID: "s4", RequiredQualification: "nurse",
		Start: at(1, 20), End: at(2, 4), EmployeeID: "A"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected rest error, got %v", err)
	}
	// 间隔正好 8 小时，合法
	if _, err := s.ScheduleShift(Shift{ID: "s5", RequiredQualification: "nurse",
		Start: at(2, 0), End: at(2, 8), EmployeeID: "A"}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestCreateSwapRequestValidatesWholeSchedule(t *testing.T) {
	s := newService(t)
	mustEmployee(t, s, "A", "nurse")
	mustEmployee(t, s, "B", "nurse")
	mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))
	mustShift(t, s, "s2", "B", "nurse", at(3, 8), at(3, 16))
	// B 另有一个班次，换班后会与 s1 冲突（休息不足）——逐条校验原班次发现不了。
	mustShift(t, s, "s3", "B", "nurse", at(1, 20), at(2, 4))

	_, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation error against full schedule, got %v", err)
	}
}

func TestCreateSwapRequestQualification(t *testing.T) {
	s := newService(t)
	mustEmployee(t, s, "A", "nurse")
	mustEmployee(t, s, "B", "doctor") // B 没有 nurse 资质
	mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))
	mustShift(t, s, "s2", "B", "doctor", at(2, 8), at(2, 16))

	_, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected qualification error, got %v", err)
	}
}

func TestCreateSwapRequestRotationRules(t *testing.T) {
	s := newService(t)
	mustEmployee(t, s, "A", "nurse")
	mustEmployee(t, s, "B", "nurse")
	mustEmployee(t, s, "C", "nurse")
	mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))
	mustShift(t, s, "s2", "B", "nurse", at(2, 8), at(2, 16))
	mustShift(t, s, "s3", "C", "nurse", at(3, 8), at(3, 16))

	// 只有一个参与者：不构成轮换
	if _, err := s.CreateSwapRequest([]Move{{ShiftID: "s1", ToEmployeeID: "A"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected rotation error, got %v", err)
	}
	// 三方中只有两方轮换、第三方只收不给：不构成轮换
	if _, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "C"},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected rotation error, got %v", err)
	}
	// 同一班次出现两次
	if _, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s1", ToEmployeeID: "C"},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected duplicate shift error, got %v", err)
	}
	// 五人轮换超出上限
	for i, id := range []string{"D", "E"} {
		mustEmployee(t, s, id, "nurse")
		mustShift(t, s, fmt.Sprintf("s%d", 4+i), id, "nurse", at(4+i, 8), at(4+i, 16))
	}
	_, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "C"},
		{ShiftID: "s3", ToEmployeeID: "D"},
		{ShiftID: "s4", ToEmployeeID: "E"},
		{ShiftID: "s5", ToEmployeeID: "A"},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected participant count error, got %v", err)
	}
}

func TestConsentCompletesSwapAtomically(t *testing.T) {
	s := newService(t)
	setupRotation(t, s)

	req, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(req.Snapshots) != 2 {
		t.Fatalf("expected 2 frozen snapshots, got %d", len(req.Snapshots))
	}

	// 第一个人同意后班次不变
	if _, err := s.Consent(req.ID, "A"); err != nil {
		t.Fatalf("consent A: %v", err)
	}
	if got := s.ListShifts("A"); len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("schedule changed before all consents: %+v", got)
	}

	// 最后一个人同意后一次性完成
	final, err := s.Consent(req.ID, "B")
	if err != nil {
		t.Fatalf("consent B: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", final.Status)
	}
	aShifts, bShifts := s.ListShifts("A"), s.ListShifts("B")
	if len(aShifts) != 1 || aShifts[0].ID != "s2" {
		t.Fatalf("A should own s2, got %+v", aShifts)
	}
	if len(bShifts) != 1 || bShifts[0].ID != "s1" {
		t.Fatalf("B should own s1, got %+v", bShifts)
	}
	if aShifts[0].Version != 2 || bShifts[0].Version != 2 {
		t.Fatalf("versions should be bumped to 2, got %d / %d", aShifts[0].Version, bShifts[0].Version)
	}

	// 历史事件完整：创建、两次同意、完成
	evs := s.Events(req.ID)
	want := []EventType{EventRequestCreated, EventConsented, EventConsented, EventCompleted}
	if len(evs) != len(want) {
		t.Fatalf("expected %d events, got %d", len(want), len(evs))
	}
	for i, w := range want {
		if evs[i].Type != w {
			t.Fatalf("event %d: expected %s, got %s", i, w, evs[i].Type)
		}
	}
	if len(evs[0].Snapshots) != 2 || len(evs[3].Moves) != 2 {
		t.Fatalf("snapshot and final moves should be persisted in events")
	}
}

func TestDuplicateConsentIsStable(t *testing.T) {
	s := newService(t)
	setupRotation(t, s)
	req, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	first, err := s.Consent(req.ID, "A")
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	second, err := s.Consent(req.ID, "A")
	if err != nil {
		t.Fatalf("duplicate consent should be a stable result, got %v", err)
	}
	if first.Status != second.Status || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("duplicate consent advanced state: %+v vs %+v", first, second)
	}
	if n := len(s.Events(req.ID)); n != 2 { // created + 1 consent
		t.Fatalf("duplicate consent appended events, now %d", n)
	}

	// 完成后再次同意：返回稳定结果，不重复推进
	if _, err := s.Consent(req.ID, "B"); err != nil {
		t.Fatalf("consent B: %v", err)
	}
	again, err := s.Consent(req.ID, "A")
	if err != nil {
		t.Fatalf("consent after completion should be stable, got %v", err)
	}
	if again.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", again.Status)
	}
	if n := len(s.Events(req.ID)); n != 4 {
		t.Fatalf("no new events expected after completion, got %d", n)
	}
	if v := s.ListShifts("A")[0].Version; v != 2 {
		t.Fatalf("version advanced twice: %d", v)
	}
}

func TestRejectAndWithdrawEndRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		act    func(s *Service, id string) (*SwapRequest, error)
		status RequestStatus
	}{
		{"reject", func(s *Service, id string) (*SwapRequest, error) { return s.Reject(id, "B") }, StatusRejected},
		{"withdraw", func(s *Service, id string) (*SwapRequest, error) { return s.Withdraw(id, "A") }, StatusWithdrawn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t)
			setupRotation(t, s)
			req, err := s.CreateSwapRequest([]Move{
				{ShiftID: "s1", ToEmployeeID: "B"},
				{ShiftID: "s2", ToEmployeeID: "A"},
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if _, err := s.Consent(req.ID, "A"); err != nil {
				t.Fatalf("consent: %v", err)
			}
			closed, err := tc.act(s, req.ID)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if closed.Status != tc.status {
				t.Fatalf("expected %s, got %s", tc.status, closed.Status)
			}
			// 原排班保持不变
			if got := s.ListShifts("A"); len(got) != 1 || got[0].ID != "s1" || got[0].Version != 1 {
				t.Fatalf("schedule changed after %s: %+v", tc.name, got)
			}
			// 之后再同意返回已结束错误
			if _, err := s.Consent(req.ID, "B"); !errors.Is(err, ErrRequestClosed) {
				t.Fatalf("expected closed error, got %v", err)
			}
		})
	}
}

func TestNonParticipantCannotAct(t *testing.T) {
	s := newService(t)
	setupRotation(t, s)
	mustEmployee(t, s, "C", "nurse")
	req, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Consent(req.ID, "C"); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("expected not-participant, got %v", err)
	}
	if _, err := s.Withdraw(req.ID, "C"); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("expected not-participant, got %v", err)
	}
}

func TestStaleVersionFailsAfterOtherRequestCommits(t *testing.T) {
	s := newService(t)
	mustEmployee(t, s, "A", "nurse")
	mustEmployee(t, s, "B", "nurse")
	mustEmployee(t, s, "C", "nurse")
	mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))
	mustShift(t, s, "s2", "B", "nurse", at(2, 8), at(2, 16))
	mustShift(t, s, "s3", "C", "nurse", at(3, 8), at(3, 16))

	// 旧申请：A <-> B
	oldReq, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if err != nil {
		t.Fatalf("create old: %v", err)
	}
	// 新申请：B <-> C，共享班次 s2
	newReq, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s2", ToEmployeeID: "C"},
		{ShiftID: "s3", ToEmployeeID: "B"},
	})
	if err != nil {
		t.Fatalf("create new: %v", err)
	}

	// 旧申请先收到 A 的同意，随后新申请抢先完成
	if _, err := s.Consent(oldReq.ID, "A"); err != nil {
		t.Fatalf("old consent A: %v", err)
	}
	if _, err := s.Consent(newReq.ID, "B"); err != nil {
		t.Fatalf("new consent B: %v", err)
	}
	if _, err := s.Consent(newReq.ID, "C"); err != nil {
		t.Fatalf("new consent C: %v", err)
	}

	// 旧申请收齐同意，但必须因版本不一致而失败
	final, err := s.Consent(oldReq.ID, "B")
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected stale version error, got %v", err)
	}
	if final.Status != StatusFailed {
		t.Fatalf("expected failed, got %s", final.Status)
	}
	// 新排班不被覆盖：s2 属于 C，s1 仍属于 A
	if got := s.ListShifts("C"); len(got) != 1 || got[0].ID != "s2" {
		t.Fatalf("new schedule was overwritten: %+v", got)
	}
	if got := s.ListShifts("A"); len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("A should still own s1: %+v", got)
	}
	// 旧申请上的重复同意返回稳定结果
	again, err := s.Consent(oldReq.ID, "A")
	if err != nil {
		t.Fatalf("duplicate consent on failed request should be stable, got %v", err)
	}
	if again.Status != StatusFailed {
		t.Fatalf("expected failed, got %s", again.Status)
	}
}

func TestConcurrentLastConsentAndWithdraw(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := newService(t)
		setupRotation(t, s)
		req, err := s.CreateSwapRequest([]Move{
			{ShiftID: "s1", ToEmployeeID: "B"},
			{ShiftID: "s2", ToEmployeeID: "A"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.Consent(req.ID, "A"); err != nil {
			t.Fatalf("consent A: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.Consent(req.ID, "B") }()
		go func() { defer wg.Done(); _, _ = s.Withdraw(req.ID, "A") }()
		wg.Wait()

		final, err := s.GetRequest(req.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		aShifts, bShifts := s.ListShifts("A"), s.ListShifts("B")
		switch final.Status {
		case StatusCompleted:
			// 全部交换，不允许只换一半
			if aShifts[0].ID != "s2" || bShifts[0].ID != "s1" {
				t.Fatalf("iter %d: partial swap: A=%+v B=%+v", i, aShifts, bShifts)
			}
		case StatusWithdrawn:
			// 完全不变
			if aShifts[0].ID != "s1" || bShifts[0].ID != "s2" {
				t.Fatalf("iter %d: withdrawn but schedule changed: A=%+v B=%+v", i, aShifts, bShifts)
			}
		default:
			t.Fatalf("iter %d: unexpected status %s", i, final.Status)
		}
	}
}

func TestFourWayRotation(t *testing.T) {
	s := newService(t)
	ids := []string{"A", "B", "C", "D"}
	for i, id := range ids {
		mustEmployee(t, s, id, "nurse")
		mustShift(t, s, fmt.Sprintf("s%d", i+1), id, "nurse", at(i+1, 8), at(i+1, 16))
	}
	req, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "C"},
		{ShiftID: "s3", ToEmployeeID: "D"},
		{ShiftID: "s4", ToEmployeeID: "A"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := len(req.Participants()); got != 4 {
		t.Fatalf("expected 4 participants, got %d", got)
	}
	for _, id := range ids {
		if _, err := s.Consent(req.ID, id); err != nil {
			t.Fatalf("consent %s: %v", id, err)
		}
	}
	final, _ := s.GetRequest(req.ID)
	if final.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", final.Status)
	}
	want := map[string]string{"A": "s4", "B": "s1", "C": "s2", "D": "s3"}
	for emp, shiftID := range want {
		if got := s.ListShifts(emp); len(got) != 1 || got[0].ID != shiftID {
			t.Fatalf("%s should own %s, got %+v", emp, shiftID, got)
		}
	}
}

func TestListRequestsFilter(t *testing.T) {
	s := newService(t)
	setupRotation(t, s)
	mustEmployee(t, s, "C", "nurse")
	mustShift(t, s, "s3", "C", "nurse", at(3, 8), at(3, 16))

	r1, _ := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	r2, _ := s.CreateSwapRequest([]Move{
		{ShiftID: "s2", ToEmployeeID: "C"},
		{ShiftID: "s3", ToEmployeeID: "B"},
	})
	_, _ = s.Consent(r1.ID, "A")
	_, _ = s.Consent(r1.ID, "B")

	if got := s.ListRequests(RequestFilter{EmployeeID: "C"}); len(got) != 1 || got[0].ID != r2.ID {
		t.Fatalf("filter by employee: %+v", got)
	}
	if got := s.ListRequests(RequestFilter{Status: StatusCompleted}); len(got) != 1 || got[0].ID != r1.ID {
		t.Fatalf("filter by status: %+v", got)
	}
	if got := s.ListRequests(RequestFilter{}); len(got) != 2 {
		t.Fatalf("filter all: %+v", got)
	}
}
