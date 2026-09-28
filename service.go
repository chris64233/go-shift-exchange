package goshiftexchange

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrRequestClosed  = errors.New("swap request is already closed")
	ErrNotParticipant = errors.New("employee is not a participant of the request")
	ErrStaleVersion   = errors.New("shift version changed since the request was submitted")
	ErrValidation     = errors.New("schedule validation failed")
	ErrConflict       = errors.New("conflict with existing data")
)

// Service 提供排班与换班的全部业务操作。所有公开方法都是并发安全的。
type Service struct {
	store   *Store
	minRest time.Duration
}

// NewService 基于 Store 创建服务。minRest 为同一员工相邻班次之间的最短休息间隔。
func NewService(store *Store, minRest time.Duration) *Service {
	return &Service{store: store, minRest: minRest}
}

// RegisterEmployee 登记员工及其资质。
func (s *Service) RegisterEmployee(e Employee) error {
	if e.ID == "" {
		return fmt.Errorf("%w: employee id is required", ErrValidation)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if _, ok := s.store.state.Employees[e.ID]; ok {
		return fmt.Errorf("%w: employee %s already exists", ErrConflict, e.ID)
	}
	cp := e
	cp.Qualifications = append([]string(nil), e.Qualifications...)
	s.store.state.Employees[e.ID] = &cp
	return s.store.saveLocked()
}

// GetEmployee 查询员工。
func (s *Service) GetEmployee(id string) (Employee, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	e, ok := s.store.state.Employees[id]
	if !ok {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrNotFound, id)
	}
	return *e, nil
}

// ScheduleShift 安排班次。班次归属员工必须持有岗位所需资质，
// 且加入该班次后该员工的完整排班仍满足不重叠与最短休息间隔。
func (s *Service) ScheduleShift(sh Shift) (Shift, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	emp, ok := s.store.state.Employees[sh.EmployeeID]
	if !ok {
		return Shift{}, fmt.Errorf("%w: employee %s", ErrNotFound, sh.EmployeeID)
	}
	if sh.ID == "" {
		return Shift{}, fmt.Errorf("%w: shift id is required", ErrValidation)
	}
	if _, dup := s.store.state.Shifts[sh.ID]; dup {
		return Shift{}, fmt.Errorf("%w: shift %s already exists", ErrConflict, sh.ID)
	}
	if !sh.End.After(sh.Start) {
		return Shift{}, fmt.Errorf("%w: shift %s end must be after start", ErrValidation, sh.ID)
	}
	shifts := shiftsOf(s.store.state, sh.EmployeeID)
	shifts = append(shifts, &sh)
	if err := validateSchedule(emp, shifts, s.minRest); err != nil {
		return Shift{}, err
	}
	sh.Version = 1
	s.store.state.Shifts[sh.ID] = &sh
	if err := s.store.saveLocked(); err != nil {
		return Shift{}, err
	}
	return sh, nil
}

// ListShifts 查询某员工的全部班次（按开始时间排序）；employeeID 为空时返回全部班次。
func (s *Service) ListShifts(employeeID string) []Shift {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []Shift
	for _, sh := range s.store.state.Shifts {
		if employeeID == "" || sh.EmployeeID == employeeID {
			out = append(out, *sh)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// CreateSwapRequest 创建换班申请。轮换关系由若干 Move 组成：
// 每个 Move 把一个班次转交给另一名员工，全部参与者（2-4 人）必须既交出又接收班次，
// 即 Move 集合必须构成轮换。提交时冻结所涉班次的当前版本，
// 并对换班后的完整排班做整体校验（资质、不重叠、最短休息）。
func (s *Service) CreateSwapRequest(moves []Move) (*SwapRequest, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	st := s.store.state

	if len(moves) == 0 {
		return nil, fmt.Errorf("%w: at least one move is required", ErrValidation)
	}
	seenShift := map[string]bool{}
	gives := map[string]int{}
	receives := map[string]int{}
	frozen := make([]Move, len(moves))
	snaps := make([]ShiftSnapshot, 0, len(moves))

	for i, m := range moves {
		sh, ok := st.Shifts[m.ShiftID]
		if !ok {
			return nil, fmt.Errorf("%w: shift %s", ErrNotFound, m.ShiftID)
		}
		if seenShift[m.ShiftID] {
			return nil, fmt.Errorf("%w: shift %s appears in multiple moves", ErrValidation, m.ShiftID)
		}
		seenShift[m.ShiftID] = true
		if _, ok := st.Employees[m.ToEmployeeID]; !ok {
			return nil, fmt.Errorf("%w: employee %s", ErrNotFound, m.ToEmployeeID)
		}
		if sh.EmployeeID == m.ToEmployeeID {
			return nil, fmt.Errorf("%w: shift %s already belongs to %s", ErrValidation, m.ShiftID, m.ToEmployeeID)
		}
		frozen[i] = Move{ShiftID: m.ShiftID, FromEmployeeID: sh.EmployeeID, ToEmployeeID: m.ToEmployeeID}
		snaps = append(snaps, ShiftSnapshot{ShiftID: sh.ID, Version: sh.Version, EmployeeID: sh.EmployeeID})
		gives[sh.EmployeeID]++
		receives[m.ToEmployeeID]++
	}

	participants := map[string]bool{}
	for id := range gives {
		participants[id] = true
	}
	for id := range receives {
		participants[id] = true
	}
	if len(participants) < 2 || len(participants) > 4 {
		return nil, fmt.Errorf("%w: a swap request must involve 2 to 4 employees, got %d", ErrValidation, len(participants))
	}
	for id := range participants {
		if gives[id] == 0 || receives[id] == 0 {
			return nil, fmt.Errorf("%w: moves must form a rotation; employee %s does not both give and receive a shift", ErrValidation, id)
		}
	}

	// 模拟换班后的完整排班并整体校验。
	if err := s.validateMovesLocked(frozen); err != nil {
		return nil, err
	}

	now := time.Now()
	consents := map[string]bool{}
	for id := range participants {
		consents[id] = false
	}
	req := &SwapRequest{
		ID:        s.store.nextIDLocked("req"),
		Moves:     frozen,
		Snapshots: snaps,
		Consents:  consents,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.Requests[req.ID] = req
	st.Events = append(st.Events, Event{
		Type:      EventRequestCreated,
		RequestID: req.ID,
		At:        now,
		Moves:     append([]Move(nil), frozen...),
		Snapshots: append([]ShiftSnapshot(nil), snaps...),
	})
	if err := s.store.saveLocked(); err != nil {
		return nil, err
	}
	return copyRequest(req), nil
}

// Consent 参与者同意申请。重复同意返回当前的稳定结果，不重复推进状态。
// 当最后一位参与者同意时，在同一把锁内原子地完成全部换班；
// 若此时班次版本与快照不一致，或换班后的完整排班不再合法，申请失败且排班不变。
func (s *Service) Consent(requestID, employeeID string) (*SwapRequest, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	st := s.store.state

	req, ok := st.Requests[requestID]
	if !ok {
		return nil, fmt.Errorf("%w: request %s", ErrNotFound, requestID)
	}
	consented, participant := req.Consents[employeeID]
	if !participant {
		return nil, fmt.Errorf("%w: %s", ErrNotParticipant, employeeID)
	}
	// 重复同意：直接返回稳定结果。
	if consented {
		return copyRequest(req), nil
	}
	if req.Status != StatusPending {
		return copyRequest(req), fmt.Errorf("%w: %s", ErrRequestClosed, req.ID)
	}

	now := time.Now()
	req.Consents[employeeID] = true
	req.UpdatedAt = now
	st.Events = append(st.Events, Event{Type: EventConsented, RequestID: req.ID, Actor: employeeID, At: now})

	for _, c := range req.Consents {
		if !c {
			if err := s.store.saveLocked(); err != nil {
				return nil, err
			}
			return copyRequest(req), nil
		}
	}

	// 全员已同意：校验版本快照，然后原子提交。
	if err := checkSnapshots(st, req); err != nil {
		req.Status = StatusFailed
		req.Reason = err.Error()
		req.UpdatedAt = now
		st.Events = append(st.Events, Event{Type: EventFailed, RequestID: req.ID, At: now, Detail: err.Error()})
		if saveErr := s.store.saveLocked(); saveErr != nil {
			return nil, saveErr
		}
		return copyRequest(req), err
	}
	if err := s.validateMovesLocked(req.Moves); err != nil {
		req.Status = StatusFailed
		req.Reason = err.Error()
		req.UpdatedAt = now
		st.Events = append(st.Events, Event{Type: EventFailed, RequestID: req.ID, At: now, Detail: err.Error()})
		if saveErr := s.store.saveLocked(); saveErr != nil {
			return nil, saveErr
		}
		return copyRequest(req), err
	}
	for _, m := range req.Moves {
		sh := st.Shifts[m.ShiftID]
		sh.EmployeeID = m.ToEmployeeID
		sh.Version++
	}
	req.Status = StatusCompleted
	req.UpdatedAt = now
	st.Events = append(st.Events, Event{
		Type:      EventCompleted,
		RequestID: req.ID,
		At:        now,
		Moves:     append([]Move(nil), req.Moves...),
	})
	if err := s.store.saveLocked(); err != nil {
		return nil, err
	}
	return copyRequest(req), nil
}

// Reject 参与者拒绝申请，申请立即结束，排班保持不变。
func (s *Service) Reject(requestID, employeeID string) (*SwapRequest, error) {
	return s.closeRequest(requestID, employeeID, StatusRejected, EventRejected)
}

// Withdraw 参与者撤回申请，申请立即结束，排班保持不变。
func (s *Service) Withdraw(requestID, employeeID string) (*SwapRequest, error) {
	return s.closeRequest(requestID, employeeID, StatusWithdrawn, EventWithdrawn)
}

func (s *Service) closeRequest(requestID, employeeID string, status RequestStatus, ev EventType) (*SwapRequest, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	st := s.store.state

	req, ok := st.Requests[requestID]
	if !ok {
		return nil, fmt.Errorf("%w: request %s", ErrNotFound, requestID)
	}
	if _, participant := req.Consents[employeeID]; !participant {
		return nil, fmt.Errorf("%w: %s", ErrNotParticipant, employeeID)
	}
	if req.Status != StatusPending {
		return copyRequest(req), fmt.Errorf("%w: %s", ErrRequestClosed, req.ID)
	}
	now := time.Now()
	req.Status = status
	req.UpdatedAt = now
	st.Events = append(st.Events, Event{Type: ev, RequestID: req.ID, Actor: employeeID, At: now})
	if err := s.store.saveLocked(); err != nil {
		return nil, err
	}
	return copyRequest(req), nil
}

// GetRequest 查询单份申请。
func (s *Service) GetRequest(id string) (*SwapRequest, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	req, ok := s.store.state.Requests[id]
	if !ok {
		return nil, fmt.Errorf("%w: request %s", ErrNotFound, id)
	}
	return copyRequest(req), nil
}

// ListRequests 按过滤条件查询历史申请（按创建时间排序）。
func (s *Service) ListRequests(f RequestFilter) []SwapRequest {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []SwapRequest
	for _, req := range s.store.state.Requests {
		if f.Status != "" && req.Status != f.Status {
			continue
		}
		if f.EmployeeID != "" {
			if _, ok := req.Consents[f.EmployeeID]; !ok {
				continue
			}
		}
		out = append(out, *copyRequest(req))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Events 查询历史事件；requestID 为空时返回全部事件。
func (s *Service) Events(requestID string) []Event {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []Event
	for _, ev := range s.store.state.Events {
		if requestID == "" || ev.RequestID == requestID {
			out = append(out, ev)
		}
	}
	return out
}

// checkSnapshots 校验申请冻结的版本快照与当前班次版本一致。
func checkSnapshots(st *state, req *SwapRequest) error {
	for _, snap := range req.Snapshots {
		sh, ok := st.Shifts[snap.ShiftID]
		if !ok {
			return fmt.Errorf("%w: shift %s no longer exists", ErrStaleVersion, snap.ShiftID)
		}
		if sh.Version != snap.Version {
			return fmt.Errorf("%w: shift %s is at version %d, request froze version %d",
				ErrStaleVersion, snap.ShiftID, sh.Version, snap.Version)
		}
	}
	return nil
}

// validateMovesLocked 在当前排班基础上模拟应用 moves，
// 对所有受影响员工的完整排班做整体校验。
func (s *Service) validateMovesLocked(moves []Move) error {
	st := s.store.state
	// 换班后每个班次的归属。
	assignee := map[string]string{}
	affected := map[string]bool{}
	for _, m := range moves {
		sh := st.Shifts[m.ShiftID]
		if sh == nil {
			return fmt.Errorf("%w: shift %s", ErrNotFound, m.ShiftID)
		}
		assignee[m.ShiftID] = m.ToEmployeeID
		affected[sh.EmployeeID] = true
		affected[m.ToEmployeeID] = true
	}
	for empID := range affected {
		emp := st.Employees[empID]
		if emp == nil {
			return fmt.Errorf("%w: employee %s", ErrNotFound, empID)
		}
		var shifts []*Shift
		for _, sh := range st.Shifts {
			to, moved := assignee[sh.ID]
			if (!moved && sh.EmployeeID == empID) || (moved && to == empID) {
				shifts = append(shifts, sh)
			}
		}
		if err := validateSchedule(emp, shifts, s.minRest); err != nil {
			return err
		}
	}
	return nil
}

// shiftsOf 返回某员工当前的全部班次。
func shiftsOf(st *state, employeeID string) []*Shift {
	var out []*Shift
	for _, sh := range st.Shifts {
		if sh.EmployeeID == employeeID {
			out = append(out, sh)
		}
	}
	return out
}

// validateSchedule 对单个员工的完整排班做整体校验：
// 每个班次要求员工持有对应资质；班次两两不得重叠；
// 相邻班次之间必须留出最短休息间隔。
func validateSchedule(emp *Employee, shifts []*Shift, minRest time.Duration) error {
	sorted := append([]*Shift(nil), shifts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	for _, sh := range sorted {
		if !emp.HasQualification(sh.RequiredQualification) {
			return fmt.Errorf("%w: employee %s lacks qualification %q required by shift %s",
				ErrValidation, emp.ID, sh.RequiredQualification, sh.ID)
		}
	}
	for i := 0; i+1 < len(sorted); i++ {
		cur, next := sorted[i], sorted[i+1]
		if next.Start.Before(cur.End) {
			return fmt.Errorf("%w: shifts %s and %s overlap for employee %s",
				ErrValidation, cur.ID, next.ID, emp.ID)
		}
		if next.Start.Sub(cur.End) < minRest {
			return fmt.Errorf("%w: rest between shifts %s and %s for employee %s is %s, less than required %s",
				ErrValidation, cur.ID, next.ID, emp.ID, next.Start.Sub(cur.End), minRest)
		}
	}
	return nil
}

// copyRequest 返回申请的深拷贝，避免调用方修改内部状态。
func copyRequest(r *SwapRequest) *SwapRequest {
	cp := *r
	cp.Moves = append([]Move(nil), r.Moves...)
	cp.Snapshots = append([]ShiftSnapshot(nil), r.Snapshots...)
	cp.Consents = make(map[string]bool, len(r.Consents))
	for k, v := range r.Consents {
		cp.Consents[k] = v
	}
	return &cp
}
