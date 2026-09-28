package goshiftexchange

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 员工与资质
// ---------------------------------------------------------------------------

func TestRegisterEmployee_DuplicateAndQualifications(t *testing.T) {
	f := newFixture(t)

	if _, err := f.svc.RegisterEmployee("alice", "Dup", nil); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate register: want ErrAlreadyExists, got %v", err)
	}
	if _, err := f.svc.RegisterEmployee(" ", "Blank", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank id: want ErrInvalidInput, got %v", err)
	}

	// 追加资质去重且幂等。
	if err := f.svc.AddQualification("alice", "B", "B", "C"); err != nil {
		t.Fatalf("AddQualification: %v", err)
	}
	if err := f.svc.AddQualification("alice", "B"); err != nil { // 全部已存在，不产生事件
		t.Fatalf("AddQualification idempotent: %v", err)
	}
	emp, err := f.svc.GetEmployee("alice")
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	if got := fmt.Sprint(emp.Positions); got != "[A B C]" {
		t.Fatalf("positions = %s, want [A B C]", got)
	}
	if err := f.svc.AddQualification("ghost", "A"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown employee: want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 排班校验：资质 / 重叠 / 最短休息
// ---------------------------------------------------------------------------

func TestScheduleShift_RejectsUnqualifiedOverlapAndShortRest(t *testing.T) {
	f := newFixture(t)

	// 资质不符：alice 只有 A，不能排 B 岗。
	_, err := f.svc.ScheduleShift(Shift{
		ID: "bad1", Position: "B", EmployeeID: "alice",
		Start: dayTime(10, 8, 0), End: dayTime(10, 16, 0),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unqualified: want validation error, got %v", err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) != 1 {
		t.Fatalf("want single qualification violation, got %v", err)
	}

	// 时间重叠：carol 已在 10-03 08:00–16:00 有 s3。
	_, err = f.svc.ScheduleShift(Shift{
		ID: "bad2", Position: "A", EmployeeID: "carol",
		Start: dayTime(3, 12, 0), End: dayTime(3, 20, 0),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("overlap: want validation error, got %v", err)
	}

	// 休息不足：s3 结束于 10-03 16:00，新班 10-03 22:00 开始，间隔 6h < 8h。
	_, err = f.svc.ScheduleShift(Shift{
		ID: "bad3", Position: "A", EmployeeID: "carol",
		Start: dayTime(3, 22, 0), End: dayTime(4, 6, 0),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short rest: want validation error, got %v", err)
	}

	// 端点间隔恰好 8h 合法。
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "ok1", Position: "A", EmployeeID: "carol",
		Start: dayTime(4, 0, 0), End: dayTime(4, 8, 0),
	}); err != nil {
		t.Fatalf("exactly 8h rest should be legal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 申请创建：2~4 人、闭环轮换、快照冻结
// ---------------------------------------------------------------------------

func TestCreateSwapRequest_ValidatesShape(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name      string
		rotations []Rotation
		want      error
	}{
		{
			name:      "only one rotation",
			rotations: []Rotation{{"s1", "bob"}},
			want:      ErrInvalidInput,
		},
		{
			name: "shift appears twice",
			rotations: []Rotation{
				{"s1", "bob"}, {"s1", "alice"},
			},
			want: ErrInvalidInput,
		},
		{
			name: "unknown shift",
			rotations: []Rotation{
				{"s1", "bob"}, {"ghost", "alice"},
			},
			want: ErrNotFound,
		},
		{
			name: "unknown new holder",
			rotations: []Rotation{
				{"s1", "bob"}, {"s2", "ghost"},
			},
			want: ErrInvalidInput,
		},
		{
			name: "not a closed cycle - outsider receives",
			rotations: []Rotation{
				{"s1", "bob"}, {"s2", "carol"}, // carol 不持有 s1/s2
			},
			want: ErrInvalidInput,
		},
		{
			name: "shift already held by target",
			rotations: []Rotation{
				{"s1", "alice"}, {"s2", "bob"}, // 原地不动
			},
			want: ErrInvalidInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.CreateSwapRequest(tc.rotations)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateSwapRequest: want %v, got %v", tc.want, err)
			}
		})
	}

	// 超过 4 名员工的轮换同样非法。
	f.svc.RegisterEmployee("erin", "Erin", []string{"A", "B"})
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "s5", Position: "A", EmployeeID: "erin",
		Start: dayTime(5, 8, 0), End: dayTime(5, 16, 0),
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.RegisterEmployee("frank", "Frank", []string{"A", "B"})
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "s6", Position: "B", EmployeeID: "frank",
		Start: dayTime(6, 8, 0), End: dayTime(6, 16, 0),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "bob"}, {"s2", "carol"}, {"s3", "dave"},
		{"s4", "erin"}, {"s5", "frank"}, {"s6", "alice"},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("6-way rotation: want ErrInvalidInput, got %v", err)
	}
}

func TestCreateSwapRequest_FreezesSnapshots(t *testing.T) {
	f := newFixture(t)
	req, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if req.Status != SwapPending || len(req.Agreed) != 0 {
		t.Fatalf("new request: status=%s agreed=%v", req.Status, req.Agreed)
	}
	if len(req.Snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(req.Snapshots))
	}
	for _, snap := range req.Snapshots {
		if snap.Shift.Version != 1 {
			t.Fatalf("snapshot %s version = %d, want 1", snap.Shift.ID, snap.Shift.Version)
		}
	}
}

func TestCreateSwapRequest_PrevalidatesPostSwapSchedule(t *testing.T) {
	// alice 只有 A 资质：换班后她要接 B 岗的 s2，整体排班不合法，创建即拒绝。
	f := newFixture(t)
	_, err := f.svc.CreateSwapRequest([]Rotation{
		{"s2", "alice"}, {"s1", "bob"}, // bob 也无 A 资质，同样不合规
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unqualified post-swap: want validation error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 原子换班：2 / 3 / 4 人轮换一次性生效
// ---------------------------------------------------------------------------

func TestSwap_TwoPeople_AppliesAtomically(t *testing.T) {
	f := newFixture(t)
	// s1 与 s3 同为 A 岗，alice 与 carol 都有 A 资质。
	req, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 第一个同意后排班不变。
	if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.holder(t, "s1"); h != "alice" {
		t.Fatalf("s1 holder = %s, want alice before all agree", h)
	}
	if h, _ := f.holder(t, "s3"); h != "carol" {
		t.Fatalf("s3 holder = %s, want carol before all agree", h)
	}

	f.agreeAll(t, req.ID, "carol")
	got, err := f.svc.GetSwapRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SwapCompleted {
		t.Fatalf("status = %s (%s), want completed", got.Status, got.FailReason)
	}
	if got.Finalization == nil || !got.Finalization.Completed || len(got.Finalization.Changes) != 2 {
		t.Fatalf("finalization = %+v, want 2 completed changes", got.Finalization)
	}
	if h, v := f.holder(t, "s1"); h != "carol" || v != 2 {
		t.Fatalf("s1 = %s v%d, want carol v2", h, v)
	}
	if h, v := f.holder(t, "s3"); h != "alice" || v != 2 {
		t.Fatalf("s3 = %s v%d, want alice v2", h, v)
	}
}

func TestSwap_ThreeAndFourPeople(t *testing.T) {
	f := newFixture(t)
	// 全员补 A/B 资质以便跨岗位轮换。
	for _, emp := range []string{"alice", "bob"} {
		if err := f.svc.AddQualification(emp, "A", "B"); err != nil {
			t.Fatal(err)
		}
	}

	// 三人轮换：s1(alice)->bob, s2(bob)->carol, s3(carol)->alice
	req3, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "bob"}, {"s2", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatalf("3-way create: %v", err)
	}
	f.agreeAll(t, req3.ID, "alice", "bob", "carol")
	got, _ := f.svc.GetSwapRequest(req3.ID)
	if got.Status != SwapCompleted {
		t.Fatalf("3-way status = %s (%s)", got.Status, got.FailReason)
	}
	for _, want := range []struct {
		id, holder string
	}{
		{"s1", "bob"}, {"s2", "carol"}, {"s3", "alice"},
	} {
		if h, v := f.holder(t, want.id); h != want.holder || v != 2 {
			t.Fatalf("%s = %s v%d, want %s v2", want.id, h, v, want.holder)
		}
	}

	// 四人轮换：当前持有人 s1=bob s2=carol s3=alice s4=dave。
	req4, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s2", "alice"}, {"s3", "dave"}, {"s4", "bob"},
	})
	if err != nil {
		t.Fatalf("4-way create: %v", err)
	}
	if len(req4.Participants) != 4 {
		t.Fatalf("participants = %v, want 4", req4.Participants)
	}
	f.agreeAll(t, req4.ID, "bob", "carol", "alice", "dave")
	got, _ = f.svc.GetSwapRequest(req4.ID)
	if got.Status != SwapCompleted {
		t.Fatalf("4-way status = %s (%s)", got.Status, got.FailReason)
	}
	for _, want := range []struct {
		id, holder string
		version    int64
	}{
		{"s1", "carol", 3}, {"s2", "alice", 3}, {"s3", "dave", 3}, {"s4", "bob", 2},
	} {
		if h, v := f.holder(t, want.id); h != want.holder || v != want.version {
			t.Fatalf("%s = %s v%d, want %s v%d", want.id, h, v, want.holder, want.version)
		}
	}
}

// TestSwap_HolisticValidation_Overlap 证明生效校验针对换班后的完整排班，
// 而非逐条原班次：逐条看两个班次都只是“换到有资质的人”，但交换后两人各自重叠。
func TestSwap_HolisticValidation_Overlap(t *testing.T) {
	// 关闭休息约束，单独聚焦“换班后才出现”的时间重叠。
	svc, _ := NewService(NewMemoryStore(), WithMinRest(0))
	for _, e := range []struct {
		id  string
		pos []string
	}{
		{"alice", []string{"A"}}, {"carol", []string{"A"}},
	} {
		if _, err := svc.RegisterEmployee(e.id, e.id, e.pos); err != nil {
			t.Fatal(err)
		}
	}
	must := func(id, emp string, start, end time.Time) {
		t.Helper()
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: "A", EmployeeID: emp, Start: start, End: end,
		}); err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
	}
	// alice：day1 08-16、day2 08-16（不重叠）
	// carol：day1 09-17、day2 09-17（不重叠；跨员工即使重叠也合法）
	must("a1", "alice", dayTime(1, 8, 0), dayTime(1, 16, 0))
	must("a2", "alice", dayTime(2, 8, 0), dayTime(2, 16, 0))
	must("c1", "carol", dayTime(2, 9, 0), dayTime(2, 17, 0))
	must("c2", "carol", dayTime(1, 9, 0), dayTime(1, 17, 0))

	// a1<->c1 后：alice 持 a2(day2 08-16) 与 c1(day2 09-17) 重叠；
	//            carol 持 c2(day1 09-17) 与 a1(day1 08-16) 重叠。
	_, err := svc.CreateSwapRequest([]Rotation{
		{"a1", "carol"}, {"c1", "alice"},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) < 2 {
		t.Fatalf("want >=2 overlap violations against post-swap schedule, got %v", err)
	}
}

// TestSwap_HolisticValidation_RestInduced 证明最短休息也针对换班后的完整班表：
// 初始班表合法、逐条轮换也合理，交换后 alice 的相邻班次间隔只剩 6.5h。
func TestSwap_HolisticValidation_RestInduced(t *testing.T) {
	svc, _ := NewService(NewMemoryStore())
	for _, id := range []string{"alice", "carol"} {
		if _, err := svc.RegisterEmployee(id, id, []string{"A"}); err != nil {
			t.Fatal(err)
		}
	}
	must := func(id, emp string, start, end time.Time) {
		t.Helper()
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: "A", EmployeeID: emp, Start: start, End: end,
		}); err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
	}
	// alice：a1 day1 08-16，a0 day2 06-07，间隔 14h，合法。
	must("a1", "alice", dayTime(1, 8, 0), dayTime(1, 16, 0))
	must("a0", "alice", dayTime(2, 6, 0), dayTime(2, 7, 0))
	// carol：x1 day1 23:00-23:30，单班合法。
	must("x1", "carol", dayTime(1, 23, 0), dayTime(1, 23, 30))

	// a1<->x1 后 alice：x1(day1 23:00-23:30) 紧接 a0(day2 06-07)，仅 6.5h < 8h。
	_, err := svc.CreateSwapRequest([]Rotation{
		{"a1", "carol"}, {"x1", "alice"},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("rest-induced swap must be rejected, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 拒绝与撤回：原排班不变、申请终结
// ---------------------------------------------------------------------------

func TestReject_EndsRequestAndKeepsSchedule(t *testing.T) {
	f := newFixture(t)
	req, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Reject(req.ID, "carol")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if res.Request.Status != SwapRejected || res.Request.RejectedBy != "carol" {
		t.Fatalf("status=%s rejectedBy=%s", res.Request.Status, res.Request.RejectedBy)
	}
	if h, _ := f.holder(t, "s1"); h != "alice" {
		t.Fatalf("s1 changed after reject: %s", h)
	}
	if h, _ := f.holder(t, "s3"); h != "carol" {
		t.Fatalf("s3 changed after reject: %s", h)
	}

	// 终态后任何推进都被拒绝。
	if _, err := f.svc.Agree(req.ID, "carol"); err == nil {
		t.Fatal("agree after reject should fail")
	}
	if _, err := f.svc.Reject(req.ID, "alice"); err == nil {
		t.Fatal("reject after reject should fail")
	}
}

func TestWithdraw_EndsRequestAndKeepsSchedule(t *testing.T) {
	f := newFixture(t)
	req, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	// 没同意过不能撤回。
	if _, err := f.svc.Withdraw(req.ID, "carol"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("withdraw without agree: want error, got %v", err)
	}
	res, err := f.svc.Withdraw(req.ID, "alice")
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if res.Request.Status != SwapWithdrawn || res.Request.WithdrawnBy != "alice" {
		t.Fatalf("status=%s withdrawnBy=%s", res.Request.Status, res.Request.WithdrawnBy)
	}
	if len(res.Request.Agreed) != 0 {
		t.Fatalf("agreed after withdraw = %v, want empty", res.Request.Agreed)
	}
	if h, _ := f.holder(t, "s1"); h != "alice" {
		t.Fatalf("s1 changed after withdraw: %s", h)
	}
	// 同一人重复撤回：幂等。
	res2, err := f.svc.Withdraw(req.ID, "alice")
	if err != nil {
		t.Fatalf("repeat withdraw: %v", err)
	}
	if !res2.Idempotent {
		t.Fatal("repeat withdraw should be idempotent")
	}
	// 其他人在终态上撤回：非幂等地报错。
	if _, err := f.svc.Withdraw(req.ID, "carol"); err == nil {
		t.Fatal("withdraw on terminal request by other should fail")
	}
}

func TestAction_RequiresParticipant(t *testing.T) {
	f := newFixture(t)
	req, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if _, err := f.svc.Agree(req.ID, "bob"); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("agree by outsider: want ErrNotParticipant, got %v", err)
	}
	if _, err := f.svc.Reject(req.ID, "bob"); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("reject by outsider: want ErrNotParticipant, got %v", err)
	}
	if _, err := f.svc.Agree("ghost", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request: want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 重复同意幂等：返回稳定结果，不重复推进
// ---------------------------------------------------------------------------

func TestAgree_DuplicateIsIdempotent(t *testing.T) {
	f := newFixture(t)
	req, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})

	first, err := f.svc.Agree(req.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if first.Idempotent || len(first.Request.Agreed) != 1 {
		t.Fatalf("first agree: %+v", first)
	}
	second, err := f.svc.Agree(req.ID, "alice")
	if err != nil {
		t.Fatalf("duplicate agree: %v", err)
	}
	if !second.Idempotent {
		t.Fatal("duplicate agree should be idempotent")
	}
	if len(second.Request.Agreed) != 1 {
		t.Fatalf("agreed = %v, want exactly 1 entry", second.Request.Agreed)
	}

	// 收齐完成后再重复同意：终态错误（申请已结束），同样不会重复落班。
	f.agreeAll(t, req.ID, "carol")
	if _, err := f.svc.Agree(req.ID, "alice"); err == nil {
		t.Fatal("agree on completed request should fail")
	}
	if h, v := f.holder(t, "s1"); h != "carol" || v != 2 {
		t.Fatalf("s1 = %s v%d, repeated agree must not re-bump version", h, v)
	}
}

// ---------------------------------------------------------------------------
// 版本冲突：班次被其他申请先改动后，旧申请即使收齐同意也必须失败
// ---------------------------------------------------------------------------

func TestAgree_VersionConflictFailsAtomically(t *testing.T) {
	f := newFixture(t)

	// 旧申请：s1(alice,A) <-> s3(carol,A)，同岗可换。
	oldReq, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Agree(oldReq.ID, "alice"); err != nil { // 只等 carol
		t.Fatal(err)
	}

	// 另一份申请先行完成，使 s1、s3 版本变化。
	newReq, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.agreeAll(t, newReq.ID, "alice", "carol")
	got, _ := f.svc.GetSwapRequest(newReq.ID)
	if got.Status != SwapCompleted {
		t.Fatalf("newReq status = %s", got.Status)
	}
	if h, v := f.holder(t, "s1"); h != "carol" || v != 2 {
		t.Fatalf("s1 precondition = %s v%d", h, v)
	}

	// 旧申请收齐最后同意：必须因版本/持有人不一致失败，且不得覆盖新排班。
	res, err := f.svc.Agree(oldReq.ID, "carol")
	if err != nil {
		t.Fatalf("Agree: %v", err)
	}
	if res.Request.Status != SwapFailed {
		t.Fatalf("old request status = %s, want failed", res.Request.Status)
	}
	if res.Request.FailReason == "" || res.Request.Finalization == nil || res.Request.Finalization.Completed {
		t.Fatalf("expected failed finalization with reason, got %+v", res.Request.Finalization)
	}
	if h, v := f.holder(t, "s1"); h != "carol" || v != 2 {
		t.Fatalf("s1 = %s v%d, newer schedule must not be overwritten", h, v)
	}
	if h, v := f.holder(t, "s3"); h != "alice" || v != 2 {
		t.Fatalf("s3 = %s v%d, newer schedule must not be overwritten", h, v)
	}

	// 失败是终态。
	if _, err := f.svc.Agree(oldReq.ID, "alice"); err == nil {
		t.Fatal("agree after failed should error")
	}
}

// TestAgree_PostSwapInvalidation 覆盖“提交时合法、生效前因新增班次导致
// 换班后完整排班不再合法”的路径：版本未变但生效整体复核失败，排班原样保留。
func TestAgree_PostSwapInvalidation(t *testing.T) {
	svc, _ := NewService(NewMemoryStore(), WithMinRest(0))
	for _, e := range []struct {
		id  string
		pos []string
	}{
		{"alice", []string{"A", "B"}},
		{"carol", []string{"A", "B"}},
	} {
		if _, err := svc.RegisterEmployee(e.id, e.id, e.pos); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(id, pos, emp string, d int) {
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: pos, EmployeeID: emp,
			Start: dayTime(d, 8, 0), End: dayTime(d, 16, 0),
		}); err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
	}
	mk("s1", "A", "alice", 1)
	mk("s3", "B", "carol", 3)

	// 申请：alice 接 B 岗 s3、carol 接 A 岗 s1（创建时两人都有资质，合法）。
	req, err := svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	// 生效前给 alice 新增 day3 12-20 班次：此刻 alice 还不持有 s3，排得进去；
	// 但换班生效整体复核时，它与 s3(day3 08-16) 时间重叠，必须失败。
	if _, err := svc.ScheduleShift(Shift{
		ID: "late", Position: "A", EmployeeID: "alice",
		Start: dayTime(3, 12, 0), End: dayTime(3, 20, 0),
	}); err != nil {
		t.Fatalf("schedule late: %v", err)
	}
	res, err := svc.Agree(req.ID, "carol")
	if err != nil {
		t.Fatalf("Agree: %v", err)
	}
	if res.Request.Status != SwapFailed {
		t.Fatalf("status = %s, want failed due to post-swap overlap", res.Request.Status)
	}
	if sh, _ := holderOf(t, svc, "s1"); sh != "alice" {
		t.Fatalf("s1 = %s, schedule must stay unchanged", sh)
	}
	if sh, _ := holderOf(t, svc, "s3"); sh != "carol" {
		t.Fatalf("s3 = %s, schedule must stay unchanged", sh)
	}
}

func holderOf(t *testing.T, svc *Service, id string) (string, int64) {
	t.Helper()
	sh, err := svc.GetShift(id)
	if err != nil {
		t.Fatal(err)
	}
	return sh.EmployeeID, sh.Version
}

// ---------------------------------------------------------------------------
// 并发：最后一次同意与撤回同时发生，不能留下部分交换
// ---------------------------------------------------------------------------

func TestConcurrent_LastAgreeVsWithdraw(t *testing.T) {
	f := newFixture(t)
	req, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// carol 的“最后一次同意”与 alice 的“撤回同意”并发。
	var wg sync.WaitGroup
	var agreeRes, withdrawRes *ActionResult
	var agreeErr, withdrawErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		agreeRes, agreeErr = f.svc.Agree(req.ID, "carol")
	}()
	go func() {
		defer wg.Done()
		withdrawRes, withdrawErr = f.svc.Withdraw(req.ID, "alice")
	}()
	wg.Wait()

	got, err := f.svc.GetSwapRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch got.Status {
	case SwapCompleted:
		// 同意先到：两个班次必须都换完，撤回必须报终态错误。
		if withdrawErr == nil {
			t.Fatalf("completed but withdraw reported success: %+v", withdrawRes)
		}
		if h, _ := f.holder(t, "s1"); h != "carol" {
			t.Fatalf("completed but s1 = %s", h)
		}
		if h, _ := f.holder(t, "s3"); h != "alice" {
			t.Fatalf("completed but s3 = %s", h)
		}
	case SwapWithdrawn:
		// 撤回先到：同意必须报终态错误，两个班次都不能动。
		if agreeErr == nil {
			t.Fatalf("withdrawn but agree reported success: %+v", agreeRes)
		}
		if h, _ := f.holder(t, "s1"); h != "alice" {
			t.Fatalf("withdrawn but s1 = %s (partial swap!)", h)
		}
		if h, _ := f.holder(t, "s3"); h != "carol" {
			t.Fatalf("withdrawn but s3 = %s (partial swap!)", h)
		}
	default:
		t.Fatalf("unexpected status %s: agreeErr=%v withdrawErr=%v", got.Status, agreeErr, withdrawErr)
	}
}

func TestConcurrent_LastAgreeVsReject_FourWay(t *testing.T) {
	// 4 人环 + 多次重复，放大竞争窗口，验证任何时序下要么整环生效、要么排班原样。
	for iter := 0; iter < 200; iter++ {
		f := newFixture(t)
		for _, emp := range []string{"alice", "bob"} {
			if err := f.svc.AddQualification(emp, "A", "B"); err != nil {
				t.Fatal(err)
			}
		}
		req, err := f.svc.CreateSwapRequest([]Rotation{
			{"s1", "bob"}, {"s2", "carol"}, {"s3", "dave"}, {"s4", "alice"},
		})
		if err != nil {
			t.Fatalf("iter %d create: %v", iter, err)
		}
		// 先收三票，dave 的最后同意与 bob 的拒绝并发。
		for _, emp := range []string{"alice", "carol", "bob"} {
			if _, err := f.svc.Agree(req.ID, emp); err != nil {
				t.Fatalf("iter %d agree %s: %v", iter, emp, err)
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var errAgree, errReject error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errAgree = f.svc.Agree(req.ID, "dave")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errReject = f.svc.Reject(req.ID, "bob")
		}()
		close(start)
		wg.Wait()

		got, _ := f.svc.GetSwapRequest(req.ID)
		switch got.Status {
		case SwapCompleted:
			if errReject == nil {
				t.Fatalf("iter %d: completed but reject succeeded", iter)
			}
			want := map[string]string{"s1": "bob", "s2": "carol", "s3": "dave", "s4": "alice"}
			for id, holder := range want {
				if h, v := f.holder(t, id); h != holder || v != 2 {
					t.Fatalf("iter %d: completed but %s = %s v%d, want %s v2", iter, id, h, v, holder)
				}
			}
		case SwapRejected:
			if errAgree == nil {
				t.Fatalf("iter %d: rejected but agree succeeded", iter)
			}
			want := map[string]string{"s1": "alice", "s2": "bob", "s3": "carol", "s4": "dave"}
			for id, holder := range want {
				if h, v := f.holder(t, id); h != holder || v != 1 {
					t.Fatalf("iter %d: rejected but %s = %s v%d (partial swap!)", iter, id, h, v)
				}
			}
		default:
			t.Fatalf("iter %d: status = %s", iter, got.Status)
		}
	}
}

// TestConcurrent_DuplicateAgreeStable 并发重复同意绝不推进两次
// （版本号最多 +1，Agreed 中每人只出现一次）。
func TestConcurrent_DuplicateAgreeStable(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		f := newFixture(t)
		req, _ := f.svc.CreateSwapRequest([]Rotation{
			{"s1", "carol"}, {"s3", "alice"},
		})
		if _, err := f.svc.Agree(req.ID, "carol"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = f.svc.Agree(req.ID, "alice")
			}()
		}
		wg.Wait()
		got, _ := f.svc.GetSwapRequest(req.ID)
		if got.Status != SwapCompleted {
			t.Fatalf("iter %d: status = %s", iter, got.Status)
		}
		count := 0
		for _, a := range got.Agreed {
			if a == "alice" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("iter %d: alice agreed %d times", iter, count)
		}
		if _, v := f.holder(t, "s1"); v != 2 {
			t.Fatalf("iter %d: s1 version = %d, want 2", iter, v)
		}
	}
}

// ---------------------------------------------------------------------------
// 历史查询
// ---------------------------------------------------------------------------

func TestHistoryForEmployee(t *testing.T) {
	f := newFixture(t)
	req, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	f.agreeAll(t, req.ID, "alice", "carol")

	h, err := f.svc.HistoryForEmployee("alice")
	if err != nil {
		t.Fatal(err)
	}
	if h.Employee.ID != "alice" {
		t.Fatalf("history employee = %s", h.Employee.ID)
	}
	// 换班后 alice 持有 s3；s1 曾作为其申请涉及班次也在历史中。
	ids := map[string]bool{}
	for _, sh := range h.Shifts {
		ids[sh.ID] = true
	}
	if !ids["s1"] || !ids["s3"] {
		t.Fatalf("alice shift history = %v, want s1 and s3", ids)
	}
	if len(h.SwapRequests) != 1 || h.SwapRequests[0].Status != SwapCompleted {
		t.Fatalf("swap history = %+v", h.SwapRequests)
	}
	if len(h.Changes) != 2 {
		t.Fatalf("changes = %d, want 2 (gave s1, took s3)", len(h.Changes))
	}

	changes := f.svc.ListChanges()
	if len(changes) != 2 {
		t.Fatalf("global changes = %d, want 2", len(changes))
	}
	for _, c := range changes {
		if c.NewVersion != c.OldVersion+1 {
			t.Fatalf("change version bump wrong: %+v", c)
		}
	}

	if _, err := f.svc.HistoryForEmployee("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown history: want ErrNotFound, got %v", err)
	}
}
