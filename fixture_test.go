package goshiftexchange

import (
	"testing"
	"time"
)

// fixture 封装一组已登记员工与班次，供多数测试复用。
//
//	alice：仅资质 A
//	bob  ：仅资质 B
//	carol：资质 A、B
//	dave ：资质 A、B
//
// 初始班次（互相不重叠、相邻间隔均 >= 16h）：
//
//	s1 2026-10-01 08:00–16:00 岗位 A  alice
//	s2 2026-10-02 08:00–16:00 岗位 B  bob
//	s3 2026-10-03 08:00–16:00 岗位 A  carol
//	s4 2026-10-04 08:00–16:00 岗位 B  dave
type fixture struct {
	svc   *Service
	store *MemoryStore
}

func dayTime(day int, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC)
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()
	store := NewMemoryStore()
	svc, err := NewService(store, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mustRegister := func(id, name string, positions ...string) {
		t.Helper()
		if _, err := svc.RegisterEmployee(id, name, positions); err != nil {
			t.Fatalf("RegisterEmployee %s: %v", id, err)
		}
	}
	mustRegister("alice", "Alice", "A")
	mustRegister("bob", "Bob", "B")
	mustRegister("carol", "Carol", "A", "B")
	mustRegister("dave", "Dave", "A", "B")

	mustSchedule := func(id, pos, emp string, start, end time.Time) {
		t.Helper()
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: pos, EmployeeID: emp, Start: start, End: end,
		}); err != nil {
			t.Fatalf("ScheduleShift %s: %v", id, err)
		}
	}
	mustSchedule("s1", "A", "alice", dayTime(1, 8, 0), dayTime(1, 16, 0))
	mustSchedule("s2", "B", "bob", dayTime(2, 8, 0), dayTime(2, 16, 0))
	mustSchedule("s3", "A", "carol", dayTime(3, 8, 0), dayTime(3, 16, 0))
	mustSchedule("s4", "B", "dave", dayTime(4, 8, 0), dayTime(4, 16, 0))

	return &fixture{svc: svc, store: store}
}

// agreeAll 让给定参与者依次同意。
func (f *fixture) agreeAll(t *testing.T, swapID string, employees ...string) {
	t.Helper()
	for _, emp := range employees {
		res, err := f.svc.Agree(swapID, emp)
		if err != nil {
			t.Fatalf("Agree(%s, %s): %v", swapID, emp, err)
		}
		if res.Idempotent {
			t.Fatalf("Agree(%s, %s) unexpectedly idempotent", swapID, emp)
		}
	}
}

// holder 断言并返回班次当前持有人与版本。
func (f *fixture) holder(t *testing.T, shiftID string) (string, int64) {
	t.Helper()
	sh, err := f.svc.GetShift(shiftID)
	if err != nil {
		t.Fatalf("GetShift %s: %v", shiftID, err)
	}
	return sh.EmployeeID, sh.Version
}
