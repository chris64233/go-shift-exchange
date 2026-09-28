package goshiftexchange

import "time"

// 事件类型。状态只通过追加事件改变，重放事件即可重建整个系统状态。
const (
	evEmployeeRegistered = "employee_registered"
	evQualificationAdded = "qualification_added"
	evShiftScheduled     = "shift_scheduled"
	evSwapCreated        = "swap_created"
	// evSwapAgreed 在最后一次同意时携带 finalization：
	// “同意收齐 + 版本核对 + 完整复核 + 一次性落班”作为同一个事件原子持久化。
	evSwapAgreed    = "swap_agreed"
	evSwapRejected  = "swap_rejected"
	evSwapWithdrawn = "swap_withdrawn"
)

// EmployeeRegisteredPayload 员工登记。
type EmployeeRegisteredPayload struct {
	Employee Employee `json:"employee"`
}

// QualificationAddedPayload 为员工追加岗位资质。
type QualificationAddedPayload struct {
	EmployeeID string   `json:"employeeId"`
	Positions  []string `json:"positions"`
}

// ShiftScheduledPayload 安排一个新班次。
type ShiftScheduledPayload struct {
	Shift Shift `json:"shift"`
}

// SwapCreatedPayload 创建换班申请，含提交时冻结的班次快照。
type SwapCreatedPayload struct {
	Request SwapRequest `json:"request"`
}

// SwapAgreedPayload 一次同意。Finalization 仅在该次同意使申请收齐时非空。
type SwapAgreedPayload struct {
	RequestID    string        `json:"requestId"`
	EmployeeID   string        `json:"employeeId"`
	Finalization *Finalization `json:"finalization,omitempty"`
}

// SwapRejectedPayload 拒绝并终结申请。
type SwapRejectedPayload struct {
	RequestID  string `json:"requestId"`
	EmployeeID string `json:"employeeId"`
}

// SwapWithdrawnPayload 撤回同意并终结申请。
type SwapWithdrawnPayload struct {
	RequestID  string `json:"requestId"`
	EmployeeID string `json:"employeeId"`
}

// Event 是持久化的最小单元。
type Event struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"ts"`
	Payload   map[string]any `json:"payload"`
}
