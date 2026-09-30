package goshiftexchange

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultMinRest 是相邻班次之间默认的最短休息间隔（8 小时）。
const DefaultMinRest = 8 * time.Hour

// Service 是排班与原子换班的入口。所有方法对并发调用安全：
// 内部用单一互斥锁把“校验 → 事件追加 → 状态推进”包成一个临界区，
// 因此最后一次同意与撤回即使并发到达，也只会有一个终结结果，
// 绝不会出现只交换了部分员工的中间状态。
type Service struct {
	mu       sync.Mutex
	store    Store
	now      func() time.Time
	idPrefix string
	seq      int64

	minRest   time.Duration
	employees map[string]*Employee
	shifts    map[string]*Shift
	requests  map[string]*SwapRequest
}

// Option 配置 Service。
type Option func(*Service)

// WithClock 注入时钟（测试用）。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithMinRest 设置相邻班次的最短休息间隔。
func WithMinRest(d time.Duration) Option {
	return func(s *Service) { s.minRest = d }
}

// NewService 从 Store 重放全部历史事件构造服务。
func NewService(store Store, opts ...Option) (*Service, error) {
	var b [4]byte
	_, _ = rand.Read(b[:])
	s := &Service{
		store:     store,
		now:       time.Now,
		idPrefix:  hex.EncodeToString(b[:]),
		employees: map[string]*Employee{},
		shifts:    map[string]*Shift{},
		requests:  map[string]*SwapRequest{},
		minRest:   DefaultMinRest,
	}
	for _, o := range opts {
		o(s)
	}
	// 重放历史事件，并把本地计数器恢复到日志最大 Seq，保证重开后追加的
	// 事件 Seq 仍然单调递增、不与历史事件冲突。
	if err := store.Load(func(ev Event) error {
		if ev.Seq > s.seq {
			s.seq = ev.Seq
		}
		return s.apply(ev)
	}); err != nil {
		return nil, fmt.Errorf("goshiftexchange: replay event log: %w", err)
	}
	return s, nil
}

// newID 生成进程内唯一 ID。
func (s *Service) newID(kind string) string {
	s.seq++
	return fmt.Sprintf("%s-%s-%d", kind, s.idPrefix, s.seq)
}

// emit 构造事件（Seq 单调递增）。
func (s *Service) emit(typ string, payload any) Event {
	s.seq++
	raw, _ := json.Marshal(payload)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return Event{Seq: s.seq, Type: typ, Timestamp: s.now(), Payload: m}
}

// commit 追加事件并同步应用到内存状态。Append 成功但 apply 理论上不会失败
// （apply 是确定性投影），一旦发生属于不可恢复的数据损坏。
func (s *Service) commit(evs []Event) error {
	if err := s.store.Append(evs); err != nil {
		return err
	}
	for _, ev := range evs {
		if err := s.apply(ev); err != nil {
			return fmt.Errorf("goshiftexchange: apply committed event: %w", err)
		}
	}
	return nil
}

// payloadAs 把 map 形态的载荷转回具体类型。
func payloadAs[T any](m map[string]any) (*T, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// apply 是事件投影：纯函数式地把单个事件应用到内存状态，重放历史时复用同一路径。
func (s *Service) apply(ev Event) error {
	switch ev.Type {
	case evEmployeeRegistered:
		p, err := payloadAs[EmployeeRegisteredPayload](ev.Payload)
		if err != nil {
			return err
		}
		e := p.Employee
		s.employees[e.ID] = &e

	case evQualificationAdded:
		p, err := payloadAs[QualificationAddedPayload](ev.Payload)
		if err != nil {
			return err
		}
		e := s.employees[p.EmployeeID]
		exist := map[string]bool{}
		for _, pos := range e.Positions {
			exist[pos] = true
		}
		for _, pos := range p.Positions {
			if !exist[pos] {
				e.Positions = append(e.Positions, pos)
				exist[pos] = true
			}
		}

	case evShiftScheduled:
		p, err := payloadAs[ShiftScheduledPayload](ev.Payload)
		if err != nil {
			return err
		}
		sh := p.Shift
		s.shifts[sh.ID] = &sh

	case evSwapCreated:
		p, err := payloadAs[SwapCreatedPayload](ev.Payload)
		if err != nil {
			return err
		}
		req := p.Request
		s.requests[req.ID] = &req

	case evSwapAgreed:
		p, err := payloadAs[SwapAgreedPayload](ev.Payload)
		if err != nil {
			return err
		}
		req := s.requests[p.RequestID]
		if req == nil {
			return fmt.Errorf("goshiftexchange: corrupt event %d: unknown swap %q", ev.Seq, p.RequestID)
		}
		// 幂等：重放/重复同意都不重复推进。
		if !contains(req.Agreed, p.EmployeeID) {
			req.Agreed = append(req.Agreed, p.EmployeeID)
		}
		if p.Finalization != nil {
			f := *p.Finalization
			req.Finalization = &f
			if f.Completed {
				for _, ch := range f.Changes {
					sh := s.shifts[ch.ShiftID]
					sh.EmployeeID = ch.ToEmployee
					sh.Version = ch.NewVersion
				}
				req.Status = SwapCompleted
			} else {
				req.Status = SwapFailed
				req.FailReason = f.Reason
			}
			req.DecidedAt = ev.Timestamp
		}

	case evSwapRejected:
		p, err := payloadAs[SwapRejectedPayload](ev.Payload)
		if err != nil {
			return err
		}
		req := s.requests[p.RequestID]
		req.Status = SwapRejected
		req.RejectedBy = p.EmployeeID
		req.DecidedAt = ev.Timestamp

	case evSwapWithdrawn:
		p, err := payloadAs[SwapWithdrawnPayload](ev.Payload)
		if err != nil {
			return err
		}
		req := s.requests[p.RequestID]
		req.Status = SwapWithdrawn
		req.WithdrawnBy = p.EmployeeID
		// 撤回即移除该参与者此前的同意。
		req.Agreed = removeString(req.Agreed, p.EmployeeID)
		req.DecidedAt = ev.Timestamp

	default:
		return fmt.Errorf("goshiftexchange: unknown event type %q", ev.Type)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 员工与资质
// ---------------------------------------------------------------------------

// RegisterEmployee 登记一名员工；positions 为其初始岗位资质。
func (s *Service) RegisterEmployee(id, name string, positions []string) (*Employee, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: employee id is required", ErrInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.employees[id]; ok {
		return nil, fmt.Errorf("%w: employee %q", ErrAlreadyExists, id)
	}
	e := Employee{
		ID:        id,
		Name:      name,
		Positions: sortedUnique(append([]string(nil), positions...)),
		CreatedAt: s.now(),
	}
	if err := s.commit([]Event{s.emit(evEmployeeRegistered, EmployeeRegisteredPayload{Employee: e})}); err != nil {
		return nil, err
	}
	got := e.Clone()
	return &got, nil
}

// AddQualification 为已登记员工追加岗位资质（已存在的资质忽略）。
func (s *Service) AddQualification(employeeID string, positions ...string) error {
	if len(positions) == 0 {
		return fmt.Errorf("%w: no position given", ErrInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.employees[employeeID]; !ok {
		return &NotFoundError{Kind: "employee", ID: employeeID}
	}
	added := make([]string, 0, len(positions))
	exist := map[string]bool{}
	for _, p := range s.employees[employeeID].Positions {
		exist[p] = true
	}
	for _, p := range positions {
		if p == "" {
			return fmt.Errorf("%w: position must not be empty", ErrInvalidInput)
		}
		if !exist[p] {
			added = append(added, p)
			exist[p] = true
		}
	}
	if len(added) == 0 {
		return nil // 幂等：全部资质已存在
	}
	return s.commit([]Event{s.emit(evQualificationAdded, QualificationAddedPayload{
		EmployeeID: employeeID,
		Positions:  added,
	})})
}

// GetEmployee 返回员工信息的副本。
func (s *Service) GetEmployee(id string) (*Employee, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.employees[id]
	if !ok {
		return nil, &NotFoundError{Kind: "employee", ID: id}
	}
	got := e.Clone()
	return &got, nil
}

// ListEmployees 按 ID 排序返回全部员工。
func (s *Service) ListEmployees() []Employee {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Employee, 0, len(s.employees))
	for _, e := range s.employees {
		out = append(out, e.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------------------------------------------------------------------------
// 班次
// ---------------------------------------------------------------------------

// ScheduleShift 安排一个新班次，并立即校验该员工当前完整排班
// （资质、与既有班次不重叠、最短休息间隔）。
func (s *Service) ScheduleShift(sh Shift) (*Shift, error) {
	if strings.TrimSpace(sh.ID) == "" {
		return nil, fmt.Errorf("%w: shift id is required", ErrInvalidInput)
	}
	if err := validateShiftShape(sh); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.shifts[sh.ID]; ok {
		return nil, fmt.Errorf("%w: shift %q", ErrAlreadyExists, sh.ID)
	}
	if _, ok := s.employees[sh.EmployeeID]; !ok {
		return nil, &NotFoundError{Kind: "employee", ID: sh.EmployeeID}
	}
	sh.Version = 1
	sh.CreatedAt = s.now()

	prospective := make(map[string]*Shift, len(s.shifts)+1)
	for id, ex := range s.shifts {
		cp := *ex
		prospective[id] = &cp
	}
	prospective[sh.ID] = &sh
	if err := validateEmployeeSchedule(s.employeeMap(), prospective, s.minRest); err != nil {
		return nil, err
	}
	if err := s.commit([]Event{s.emit(evShiftScheduled, ShiftScheduledPayload{Shift: sh})}); err != nil {
		return nil, err
	}
	got := sh
	return &got, nil
}

// employeeMap 在持锁状态下返回员工深拷贝映射（校验器不得持有内部指针）。
func (s *Service) employeeMap() map[string]Employee {
	m := make(map[string]Employee, len(s.employees))
	for id, e := range s.employees {
		m[id] = *e
	}
	return m
}

// GetShift 返回班次副本。
func (s *Service) GetShift(id string) (*Shift, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shifts[id]
	if !ok {
		return nil, &NotFoundError{Kind: "shift", ID: id}
	}
	got := *sh
	return &got, nil
}

// ListShifts 按开始时间、ID 排序返回全部班次。
func (s *Service) ListShifts() []Shift {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Shift, 0, len(s.shifts))
	for _, sh := range s.shifts {
		out = append(out, *sh)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ---------------------------------------------------------------------------
// 换班申请
// ---------------------------------------------------------------------------

// CreateSwapRequest 创建一份两到四名员工之间的换班申请，并冻结涉及班次的当前版本。
//
// 创建时即对换班后的完整排班做一次预检（返回快照时申请者能立刻看到“这样换合法”）；
// 但预检结果不作为最终依据，最后一次同意生效时会基于最新状态重新完整复核。
func (s *Service) CreateSwapRequest(rotations []Rotation) (*SwapRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.buildSwapRequest(rotations)
	if err != nil {
		return nil, err
	}
	if err := s.commit([]Event{s.emit(evSwapCreated, SwapCreatedPayload{Request: *req})}); err != nil {
		return nil, err
	}
	got := req.Clone()
	return &got, nil
}

// buildSwapRequest 在持锁状态下校验轮换关系、冻结快照并做创建时预检。
func (s *Service) buildSwapRequest(rotations []Rotation) (*SwapRequest, error) {
	var problems []string
	if n := len(rotations); n < 2 || n > 4 {
		problems = append(problems, fmt.Sprintf("a swap must rotate 2 to 4 shifts, got %d", n))
	}

	shiftSet := map[string]bool{}
	newHolders := map[string]int{}
	holders := map[string]int{}
	for _, r := range rotations {
		if r.ShiftID == "" || r.NewEmployeeID == "" {
			problems = append(problems, "each rotation needs a shift id and a new employee id")
			continue
		}
		if shiftSet[r.ShiftID] {
			problems = append(problems, fmt.Sprintf("shift %q appears more than once", r.ShiftID))
		}
		shiftSet[r.ShiftID] = true
		newHolders[r.NewEmployeeID]++
	}

	// 解析所涉及班次、冻结快照，并统计当前持有人集合。
	snapshots := make([]ShiftSnapshot, 0, len(rotations))
	for _, r := range rotations {
		if r.ShiftID == "" {
			continue // 已在上面的问题列表中
		}
		sh, ok := s.shifts[r.ShiftID]
		if !ok {
			// 班次不存在属于实体缺失，直接返回 NotFound 而非聚合校验错误。
			return nil, &NotFoundError{Kind: "shift", ID: r.ShiftID}
		}
		if r.NewEmployeeID != "" {
			if _, ok := s.employees[r.NewEmployeeID]; !ok {
				problems = append(problems, fmt.Sprintf("employee %q not registered", r.NewEmployeeID))
			}
		}
		snapshots = append(snapshots, ShiftSnapshot{Shift: *sh})
		holders[sh.EmployeeID]++
		if sh.EmployeeID == r.NewEmployeeID {
			problems = append(problems, fmt.Sprintf("shift %q is already held by %q", r.ShiftID, r.NewEmployeeID))
		}
	}

	// 参与者 = 当前持有人集合，必须恰好 2~4 人。
	participantSet := map[string]bool{}
	for id := range holders {
		participantSet[id] = true
	}
	if len(problems) == 0 {
		if n := len(participantSet); n < 2 || n > 4 {
			problems = append(problems, fmt.Sprintf("a swap must involve 2 to 4 distinct employees, got %d", n))
		}
		// 轮换必须在参与者集合内部闭环：每个参与者交出一个班次、接一个班次。
		if len(newHolders) != len(participantSet) {
			problems = append(problems, "rotations must form a closed cycle among the current holders")
		}
		for emp, n := range newHolders {
			if !participantSet[emp] {
				problems = append(problems, fmt.Sprintf("new holder %q is not among current holders", emp))
			}
			if holders[emp] != n {
				problems = append(problems, fmt.Sprintf("employee %q would give up %d shift(s) but take %d", emp, holders[emp], n))
			}
		}
	}
	if err := validationError(problems); err != nil {
		return nil, err
	}

	participants := make([]string, 0, len(participantSet))
	for id := range participantSet {
		participants = append(participants, id)
	}
	sort.Strings(participants)
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Shift.ID < snapshots[j].Shift.ID })
	rots := append([]Rotation(nil), rotations...)
	sort.Slice(rots, func(i, j int) bool { return rots[i].ShiftID < rots[j].ShiftID })

	req := &SwapRequest{
		ID:           s.newID("swap"),
		Participants: participants,
		Rotations:    rots,
		Snapshots:    snapshots,
		Status:       SwapPending,
		CreatedAt:    s.now(),
	}

	// 创建时预检：对“换班后的完整排班”做三项校验，而不是逐条检查原班次。
	prospective, _ := buildProspectiveSchedule(s.shifts, rots)
	if err := validateEmployeeSchedule(s.employeeMap(), prospective, s.minRest); err != nil {
		return nil, err
	}
	return req, nil
}

// GetSwapRequest 返回申请副本。
func (s *Service) GetSwapRequest(id string) (*SwapRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[id]
	if !ok {
		return nil, &NotFoundError{Kind: "swap request", ID: id}
	}
	got := req.Clone()
	return &got, nil
}

// ListSwapRequests 按创建时间、ID 排序返回全部申请。
func (s *Service) ListSwapRequests() []SwapRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SwapRequest, 0, len(s.requests))
	for _, req := range s.requests {
		out = append(out, req.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ActionResult 是同意/拒绝/撤回操作的稳定结果。
type ActionResult struct {
	Request SwapRequest
	// Idempotent 为 true 表示本次调用没有推进状态：
	// 重复同意（此前已同意且申请未终结）或重复终结动作。
	Idempotent bool
}

// Agree 记录参与者同意。
//
// 当本次同意使所有参与者收齐时，在同一临界区内一次性完成：
//  1. 核对每个涉及班次的当前版本与提交时冻结版本一致、持有人未变；
//  2. 基于最新排班构造换班后的完整排班，重新做资质/不重叠/最短休息校验；
//  3. 全部通过才把所有涉及班次同时改派并推进版本（部分通过也不会先落任何班次）；
//  4. “同意 + 裁决结果”写入同一个事件持久化，崩溃重放也不会出现半截交换。
//
// 重复同意返回 Idempotent=true 且不重复推进状态。
func (s *Service) Agree(requestID, employeeID string) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, err := s.lockPendingParticipant(requestID, employeeID)
	if err != nil {
		return nil, err
	}
	if contains(req.Agreed, employeeID) {
		return &ActionResult{Request: req.Clone(), Idempotent: true}, nil
	}

	var fin *Finalization
	if len(req.Agreed)+1 == len(req.Participants) {
		fin = s.finalize(req)
	}
	payload := SwapAgreedPayload{RequestID: requestID, EmployeeID: employeeID, Finalization: fin}
	if err := s.commit([]Event{s.emit(evSwapAgreed, payload)}); err != nil {
		return nil, err
	}
	return &ActionResult{Request: s.requests[requestID].Clone()}, nil
}

// lockPendingParticipant 取出 pending 申请并校验参与者身份。
func (s *Service) lockPendingParticipant(requestID, employeeID string) (*SwapRequest, error) {
	req, ok := s.requests[requestID]
	if !ok {
		return nil, &NotFoundError{Kind: "swap request", ID: requestID}
	}
	if req.Status.Terminal() {
		return nil, &TerminalStateError{RequestID: requestID, Status: req.Status}
	}
	if !contains(req.Participants, employeeID) {
		return nil, fmt.Errorf("%w: employee %q, swap %q", ErrNotParticipant, employeeID, requestID)
	}
	return req, nil
}

// finalize 在持锁状态下执行生效裁决；它不修改任何状态，只返回裁决结果。
func (s *Service) finalize(req *SwapRequest) *Finalization {
	frozen := make(map[string]Shift, len(req.Snapshots))
	for _, snap := range req.Snapshots {
		frozen[snap.Shift.ID] = snap.Shift
	}

	// 1) 版本与持有人核对：涉及班次只要被其他申请/操作先改动过即失败。
	for _, r := range req.Rotations {
		live, ok := s.shifts[r.ShiftID]
		if !ok {
			return &Finalization{Completed: false, Reason: fmt.Sprintf("shift %q no longer exists", r.ShiftID)}
		}
		old := frozen[r.ShiftID]
		if live.Version != old.Version {
			return &Finalization{Completed: false, Reason: fmt.Sprintf(
				"shift %q version conflict: frozen at v%d, now v%d", r.ShiftID, old.Version, live.Version)}
		}
		if live.EmployeeID != old.EmployeeID {
			return &Finalization{Completed: false, Reason: fmt.Sprintf(
				"shift %q holder changed: frozen %q, now %q", r.ShiftID, old.EmployeeID, live.EmployeeID)}
		}
	}

	// 2) 构造换班后的完整排班并整体复核（不是逐条校验原班次）。
	prospective, currentHolder := buildProspectiveSchedule(s.shifts, req.Rotations)
	if err := validateEmployeeSchedule(s.employeeMap(), prospective, s.minRest); err != nil {
		return &Finalization{Completed: false, Reason: err.Error()}
	}

	// 3) 全部通过：一次性产出所有改派明细（真正落班由 apply 在同一事件中完成）。
	changes := make([]ShiftChange, 0, len(req.Rotations))
	for _, r := range req.Rotations {
		live := s.shifts[r.ShiftID]
		changes = append(changes, ShiftChange{
			ShiftID:      r.ShiftID,
			Position:     live.Position,
			FromEmployee: currentHolder[r.ShiftID],
			ToEmployee:   r.NewEmployeeID,
			OldVersion:   live.Version,
			NewVersion:   live.Version + 1,
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].ShiftID < changes[j].ShiftID })
	return &Finalization{Completed: true, Changes: changes}
}

// Reject 由参与者拒绝申请；申请立即终结为 rejected，原排班保持不变。
// 申请已被本人拒绝时重复调用返回 Idempotent=true。
func (s *Service) Reject(requestID, employeeID string) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.requests[requestID]
	if !ok {
		return nil, &NotFoundError{Kind: "swap request", ID: requestID}
	}
	// 同一拒绝者重复拒绝：幂等返回终态。
	if req.Status == SwapRejected && req.RejectedBy == employeeID {
		return &ActionResult{Request: req.Clone(), Idempotent: true}, nil
	}
	if req.Status.Terminal() {
		return nil, &TerminalStateError{RequestID: requestID, Status: req.Status}
	}
	if !contains(req.Participants, employeeID) {
		return nil, fmt.Errorf("%w: employee %q, swap %q", ErrNotParticipant, employeeID, requestID)
	}
	if err := s.commit([]Event{s.emit(evSwapRejected, SwapRejectedPayload{
		RequestID: requestID, EmployeeID: employeeID,
	})}); err != nil {
		return nil, err
	}
	return &ActionResult{Request: s.requests[requestID].Clone()}, nil
}

// Withdraw 由参与者撤回自己此前的同意；申请立即终结为 withdrawn，原排班不变。
// 尚未同意过则返回 ErrInvalidInput。申请终结后重复撤回返回 Idempotent=true。
func (s *Service) Withdraw(requestID, employeeID string) (*ActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.requests[requestID]
	if !ok {
		return nil, &NotFoundError{Kind: "swap request", ID: requestID}
	}
	if req.Status == SwapWithdrawn && req.WithdrawnBy == employeeID {
		return &ActionResult{Request: req.Clone(), Idempotent: true}, nil
	}
	if req.Status.Terminal() {
		return nil, &TerminalStateError{RequestID: requestID, Status: req.Status}
	}
	if !contains(req.Participants, employeeID) {
		return nil, fmt.Errorf("%w: employee %q, swap %q", ErrNotParticipant, employeeID, requestID)
	}
	if !contains(req.Agreed, employeeID) {
		return nil, fmt.Errorf("%w: employee %q has not agreed to swap %q", ErrInvalidInput, employeeID, requestID)
	}
	if err := s.commit([]Event{s.emit(evSwapWithdrawn, SwapWithdrawnPayload{
		RequestID: requestID, EmployeeID: employeeID,
	})}); err != nil {
		return nil, err
	}
	return &ActionResult{Request: s.requests[requestID].Clone()}, nil
}

// ---------------------------------------------------------------------------
// 历史查询
// ---------------------------------------------------------------------------

// EmployeeHistory 汇总一名员工相关的历史记录。
type EmployeeHistory struct {
	Employee       Employee      `json:"employee"`
	Qualifications []string      `json:"qualifications"`
	Shifts         []Shift       `json:"shifts"`
	SwapRequests   []SwapRequest `json:"swapRequests"`
	// Changes 是该员工作为交出方或接收方参与的最终换班变更。
	Changes []ShiftChange `json:"changes"`
}

// HistoryForEmployee 查询员工自身相关的完整历史：其班次、作为参与者的申请
// （含申请快照与最终变更）、以及最终发生的改派明细。
func (s *Service) HistoryForEmployee(employeeID string) (*EmployeeHistory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	emp, ok := s.employees[employeeID]
	if !ok {
		return nil, &NotFoundError{Kind: "employee", ID: employeeID}
	}
	h := &EmployeeHistory{
		Employee:       emp.Clone(),
		Qualifications: cloneStrings(emp.Positions),
	}
	for _, sh := range s.shifts {
		if sh.EmployeeID == employeeID || snapshotInvolves(s.requests, employeeID, sh.ID) {
			h.Shifts = append(h.Shifts, *sh)
		}
	}
	for _, req := range s.requests {
		if contains(req.Participants, employeeID) {
			h.SwapRequests = append(h.SwapRequests, req.Clone())
			if req.Finalization != nil && req.Finalization.Completed {
				for _, ch := range req.Finalization.Changes {
					if ch.FromEmployee == employeeID || ch.ToEmployee == employeeID {
						h.Changes = append(h.Changes, ch)
					}
				}
			}
		}
	}
	sort.Slice(h.Shifts, func(i, j int) bool {
		if !h.Shifts[i].Start.Equal(h.Shifts[j].Start) {
			return h.Shifts[i].Start.Before(h.Shifts[j].Start)
		}
		return h.Shifts[i].ID < h.Shifts[j].ID
	})
	sort.Slice(h.SwapRequests, func(i, j int) bool {
		if !h.SwapRequests[i].CreatedAt.Equal(h.SwapRequests[j].CreatedAt) {
			return h.SwapRequests[i].CreatedAt.Before(h.SwapRequests[j].CreatedAt)
		}
		return h.SwapRequests[i].ID < h.SwapRequests[j].ID
	})
	return h, nil
}

// snapshotInvolves 报告某班次是否出现在该员工参与的任一申请快照中
// （即使该班次最终没有派给该员工，历史里也保留冻结记录，通过申请快照体现；
// 这里仅用于把“曾被申请涉及、现已不持有”的班次纳入其可追溯范围）。
func snapshotInvolves(requests map[string]*SwapRequest, employeeID, shiftID string) bool {
	for _, req := range requests {
		if !contains(req.Participants, employeeID) {
			continue
		}
		for _, r := range req.Rotations {
			if r.ShiftID == shiftID {
				return true
			}
		}
	}
	return false
}

// ListChanges 返回所有已生效换班的最终变更明细，按班次 ID、新版本号排序。
func (s *Service) ListChanges() []ShiftChange {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ShiftChange
	for _, req := range s.requests {
		if req.Finalization == nil || !req.Finalization.Completed {
			continue
		}
		out = append(out, req.Finalization.Changes...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ShiftID != out[j].ShiftID {
			return out[i].ShiftID < out[j].ShiftID
		}
		return out[i].NewVersion < out[j].NewVersion
	})
	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func removeString(xs []string, x string) []string {
	out := xs[:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

func sortedUnique(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0]
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
