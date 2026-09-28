// Package goshiftexchange 实现多人排班场景下的原子换班申请。
//
// 一份换班申请（SwapRequest）可以描述两到四名员工之间的班次轮换；申请提交时
// 冻结所涉及班次的当前版本。只有所有参与者一致同意，换班才会一次性生效：生效前
// 会再次核对班次版本并对“换班后的完整排班”做岗位资质、时间不重叠、最短休息间隔
// 三项校验。任何人拒绝或撤回都会立即结束申请，原排班保持不变；任何并发时序下都
// 不可能出现只交换了部分员工的中间结果。
//
// 状态全部以追加事件的方式持久化（见 Store 与 FileStore），重放事件即可重建。
package goshiftexchange

import "time"

// SwapStatus 是换班申请的生命周期状态。
type SwapStatus string

const (
	// SwapPending 表示申请仍在等待所有参与者同意。
	SwapPending SwapStatus = "pending"
	// SwapCompleted 表示所有参与者已同意，且换班已一次性原子生效。
	SwapCompleted SwapStatus = "completed"
	// SwapRejected 表示有参与者拒绝，申请结束，原排班不变。
	SwapRejected SwapStatus = "rejected"
	// SwapWithdrawn 表示有参与者撤回同意，申请结束，原排班不变。
	SwapWithdrawn SwapStatus = "withdrawn"
	// SwapFailed 表示同意虽已收齐，但生效时班次版本已变化或换班后排班不再合法，
	// 申请结束，原排班不变。
	SwapFailed SwapStatus = "failed"
)

// Terminal 报告该状态是否为终态（终态申请不再接受任何状态推进）。
func (s SwapStatus) Terminal() bool {
	switch s {
	case SwapCompleted, SwapRejected, SwapWithdrawn, SwapFailed:
		return true
	default:
		return false
	}
}

// Employee 是一名员工及其持有的岗位资质。
type Employee struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Positions []string  `json:"positions"`
	CreatedAt time.Time `json:"createdAt"`
}

// Shift 是一个班次。Version 为乐观版本号：班次归属每次成功变化时 +1。
type Shift struct {
	ID         string    `json:"id"`
	Position   string    `json:"position"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	EmployeeID string    `json:"employeeId"`
	Version    int64     `json:"version"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Rotation 描述申请中的一条轮换关系：把 ShiftID 指定的班次改派给 NewEmployeeID。
type Rotation struct {
	ShiftID       string `json:"shiftId"`
	NewEmployeeID string `json:"newEmployeeId"`
}

// ShiftSnapshot 是申请提交时冻结的班次快照（含当时版本）。
type ShiftSnapshot struct {
	Shift Shift `json:"shift"`
}

// Finalization 是最后一次同意到来、同意收齐那一刻的原子裁决结果。
type Finalization struct {
	// Completed 为 true 时换班一次性生效；为 false 时申请终态为 Failed。
	Completed bool `json:"completed"`
	// Reason 在失败时给出原因（版本不一致或换班后排班校验失败）。
	Reason string `json:"reason,omitempty"`
	// Changes 为生效时真正落班的改派明细。
	Changes []ShiftChange `json:"changes,omitempty"`
}

// ShiftChange 是换班生效时一个班次的最终改派记录。
type ShiftChange struct {
	ShiftID      string `json:"shiftId"`
	Position     string `json:"position"`
	FromEmployee string `json:"fromEmployee"`
	ToEmployee   string `json:"toEmployee"`
	OldVersion   int64  `json:"oldVersion"`
	NewVersion   int64  `json:"newVersion"`
}

// SwapRequest 是一份换班申请（含提交时冻结的快照）。
type SwapRequest struct {
	ID           string          `json:"id"`
	Participants []string        `json:"participants"`
	Rotations    []Rotation      `json:"rotations"`
	Snapshots    []ShiftSnapshot `json:"snapshots"`
	Status       SwapStatus      `json:"status"`
	Agreed       []string        `json:"agreed"`
	// RejectedBy / WithdrawnBy 记录终结申请的参与者。
	RejectedBy  string `json:"rejectedBy,omitempty"`
	WithdrawnBy string `json:"withdrawnBy,omitempty"`
	// FailReason 记录同意收齐却未生效的原因。
	FailReason string `json:"failReason,omitempty"`
	// Finalization 在最后一次同意时一次性写入，裁决与落班结果都在其中。
	Finalization *Finalization `json:"finalization,omitempty"`
	CreatedAt    time.Time     `json:"createdAt"`
	DecidedAt    time.Time     `json:"decidedAt,omitempty"`
}

// clone 工具：复制字符串切片，避免调用方通过返回值改写内部状态。
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// Clone 返回员工的深拷贝。
func (e Employee) Clone() Employee {
	e.Positions = cloneStrings(e.Positions)
	return e
}

// Clone 返回班次快照的深拷贝。
func (s Shift) Clone() Shift { return s }

// Clone 返回换班申请的深拷贝。
func (r SwapRequest) Clone() SwapRequest {
	r.Participants = cloneStrings(r.Participants)
	r.Agreed = cloneStrings(r.Agreed)
	if r.Rotations != nil {
		r.Rotations = append([]Rotation(nil), r.Rotations...)
	}
	if r.Snapshots != nil {
		r.Snapshots = append([]ShiftSnapshot(nil), r.Snapshots...)
	}
	if r.Finalization != nil {
		f := *r.Finalization
		f.Changes = append([]ShiftChange(nil), r.Finalization.Changes...)
		r.Finalization = &f
	}
	return r
}
