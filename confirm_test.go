package goshiftexchange

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// 管理员取消：立即终结、原排班不变、重复取消幂等、迟到确认报终态冲突
// ---------------------------------------------------------------------------

func TestCancel_EndsRequestAndKeepsSchedule(t *testing.T) {
	f := newFixture(t)
	req, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Cancel(req.ID, "schedule adjusted by admin")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if res.Idempotent || res.Request.Status != SwapCancelled {
		t.Fatalf("cancel result = %+v", res)
	}
	if res.Request.CancelReason != "schedule adjusted by admin" {
		t.Fatalf("cancel reason = %q", res.Request.CancelReason)
	}

	// 双方班次都保持原样：不存在“甲已换、乙未换”的半成品。
	if h, v := f.holder(t, "s1"); h != "alice" || v != 1 {
		t.Fatalf("s1 = %s v%d, want alice v1", h, v)
	}
	if h, v := f.holder(t, "s3"); h != "carol" || v != 1 {
		t.Fatalf("s3 = %s v%d, want carol v1", h, v)
	}

	// 重复取消幂等。
	again, err := f.svc.Cancel(req.ID, "again")
	if err != nil || !again.Idempotent {
		t.Fatalf("repeat cancel = %+v, %v", again, err)
	}

	// 迟到的确认得到终态冲突，而不是悄悄生效。
	if _, err := f.svc.Agree(req.ID, "carol"); err == nil {
		t.Fatal("late agree after cancel should fail")
	} else {
		var term *TerminalStateError
		if !errors.As(err, &term) || term.Status != SwapCancelled {
			t.Fatalf("late agree err = %v, want TerminalStateError(cancelled)", err)
		}
	}
	if h, v := f.holder(t, "s1"); h != "alice" || v != 1 {
		t.Fatalf("s1 = %s v%d, cancelled swap must not apply", h, v)
	}
}

// TestConcurrent_LastAgreeVsCancel 反复竞争“最后一次确认”与“管理员取消”：
// 结果只能是换班整体生效或申请被取消，绝不出现只换了一边的中间态。
func TestConcurrent_LastAgreeVsCancel(t *testing.T) {
	for i := 0; i < 200; i++ {
		f := newFixture(t)
		req, _ := f.svc.CreateSwapRequest([]Rotation{
			{"s1", "carol"}, {"s3", "alice"},
		})
		if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = f.svc.Agree(req.ID, "carol") }()
		go func() { defer wg.Done(); _, _ = f.svc.Cancel(req.ID, "admin") }()
		wg.Wait()

		got, _ := f.svc.GetSwapRequest(req.ID)
		h1, v1 := f.holder(t, "s1")
		h3, v3 := f.holder(t, "s3")
		switch got.Status {
		case SwapCompleted:
			if h1 != "carol" || v1 != 2 || h3 != "alice" || v3 != 2 {
				t.Fatalf("iter %d: completed but half applied: s1=%s v%d s3=%s v%d",
					i, h1, v1, h3, v3)
			}
		case SwapCancelled:
			if h1 != "alice" || v1 != 1 || h3 != "carol" || v3 != 1 {
				t.Fatalf("iter %d: cancelled but schedule changed: s1=%s v%d s3=%s v%d",
					i, h1, v1, h3, v3)
			}
		default:
			t.Fatalf("iter %d: status = %s, want completed or cancelled", i, got.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// 岗位人数：确认时与资质、休息间隔一起整体校验，不满足则两边都不变
// ---------------------------------------------------------------------------

func TestSwap_MinStaffing_EnforcedAtomically(t *testing.T) {
	svc, _ := NewService(NewMemoryStore(), WithMinRest(0), WithMinStaffing("A", 2))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.RegisterEmployee("alice", "Alice", []string{"A", "B"})
	must(err)
	_, err = svc.RegisterEmployee("carol", "Carol", []string{"A", "B"})
	must(err)
	_, err = svc.ScheduleShift(Shift{ID: "s1", Position: "A", EmployeeID: "alice",
		Start: dayTime(1, 8, 0), End: dayTime(1, 16, 0)})
	must(err)
	_, err = svc.ScheduleShift(Shift{ID: "s2", Position: "B", EmployeeID: "carol",
		Start: dayTime(2, 8, 0), End: dayTime(2, 16, 0)})
	must(err)

	// 换班后岗位 A 只剩 carol 一人承担，低于下限 2：创建预检即拒绝。
	_, err = svc.CreateSwapRequest([]Rotation{{"s1", "carol"}, {"s2", "alice"}})
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("create err = %v, want ValidationError", err)
	}
	found := false
	for _, v := range verr.Violations {
		if strings.Contains(v, `position "A"`) && strings.Contains(v, "minimum 2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("violations = %v, want staffing violation for position A", verr.Violations)
	}

	// 下限放宽到换班后仍满足：可以创建并生效。
	svc2, _ := NewService(NewMemoryStore(), WithMinRest(0), WithMinStaffing("A", 1))
	_, err = svc2.RegisterEmployee("alice", "Alice", []string{"A", "B"})
	must(err)
	_, err = svc2.RegisterEmployee("carol", "Carol", []string{"A", "B"})
	must(err)
	_, err = svc2.ScheduleShift(Shift{ID: "s1", Position: "A", EmployeeID: "alice",
		Start: dayTime(1, 8, 0), End: dayTime(1, 16, 0)})
	must(err)
	_, err = svc2.ScheduleShift(Shift{ID: "s2", Position: "B", EmployeeID: "carol",
		Start: dayTime(2, 8, 0), End: dayTime(2, 16, 0)})
	must(err)
	req, err := svc2.CreateSwapRequest([]Rotation{{"s1", "carol"}, {"s2", "alice"}})
	must(err)
	if _, err := svc2.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	res, err := svc2.Agree(req.ID, "carol")
	must(err)
	if res.Request.Status != SwapCompleted {
		t.Fatalf("status = %s, want completed", res.Request.Status)
	}
}

// TestSwap_MinStaffing_ChangedBeforeConfirm 覆盖“创建后、确认前同一批班次被
// 另一份申请先占用”的路径：生效裁决因版本冲突失败，两边班次都不留下半成品。
func TestSwap_ConcurrentOccupation_NoHalfState(t *testing.T) {
	svc, _ := NewService(NewMemoryStore(), WithMinRest(0), WithMinStaffing("B", 1))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.RegisterEmployee("alice", "Alice", []string{"A", "B"})
	must(err)
	_, err = svc.RegisterEmployee("carol", "Carol", []string{"A", "B"})
	must(err)
	_, err = svc.ScheduleShift(Shift{ID: "s1", Position: "A", EmployeeID: "alice",
		Start: dayTime(1, 8, 0), End: dayTime(1, 16, 0)})
	must(err)
	_, err = svc.ScheduleShift(Shift{ID: "s2", Position: "B", EmployeeID: "carol",
		Start: dayTime(2, 8, 0), End: dayTime(2, 16, 0)})
	must(err)

	req, err := svc.CreateSwapRequest([]Rotation{{"s1", "carol"}, {"s2", "alice"}})
	must(err)
	if _, err := svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// 确认前另一份申请先把同一批班次换走（重复占用班次，版本推进）。
	other, err := svc.CreateSwapRequest([]Rotation{{"s2", "alice"}, {"s1", "carol"}})
	must(err)
	if _, err := svc.Agree(other.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Agree(other.ID, "carol"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Agree(req.ID, "carol")
	must(err)
	if res.Request.Status != SwapFailed || res.Request.FailReason == "" {
		t.Fatalf("status = %s reason = %q, want failed with reason",
			res.Request.Status, res.Request.FailReason)
	}
	// 本申请失败不得留下半成品：s1/s2 只反映另一份申请的一次性结果。
	sh1, _ := svc.GetShift("s1")
	sh2, _ := svc.GetShift("s2")
	if sh1.EmployeeID != "carol" || sh1.Version != 2 ||
		sh2.EmployeeID != "alice" || sh2.Version != 2 {
		t.Fatalf("s1=%s v%d s2=%s v%d, failed swap must not partially apply",
			sh1.EmployeeID, sh1.Version, sh2.EmployeeID, sh2.Version)
	}
}

// ---------------------------------------------------------------------------
// 版本冲突原因可读：冻结版本与当前版本都出现在失败原因中
// ---------------------------------------------------------------------------

func TestSwap_VersionConflict_ReasonIsUnderstandable(t *testing.T) {
	f := newFixture(t)
	oldReq, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if _, err := f.svc.Agree(oldReq.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	// 同一批班次被第二份申请先占用并完成（重复占用班次）。
	newReq, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	f.agreeAll(t, newReq.ID, "alice", "carol")

	res, err := f.svc.Agree(oldReq.ID, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if res.Request.Status != SwapFailed {
		t.Fatalf("status = %s, want failed", res.Request.Status)
	}
	reason := res.Request.FailReason
	if !strings.Contains(reason, "version conflict") ||
		!strings.Contains(reason, "v1") || !strings.Contains(reason, "v2") {
		t.Fatalf("reason = %q, want understandable version conflict with versions", reason)
	}
}
