package goshiftexchange

import "time"

// 事件类型。状态只通过追加事件改变，重放事件即可重建整个系统状态。
const (
	evEmployeeRegistered = "employee_registered"
	evQualificationAdded = "qualification_added"
	evShiftScheduled     = "shift_scheduled"
	evStaffingSet        = "staffing_requirement_set"
	evScheduleChanged    = "schedule_changed"
	evSwapCreated        = "swap_created"
	// evSwapAgreed 在最后一次同意时携带 finalization：
	// “同意收齐 + 版本核对 + 完整复核 + 一次性落班”作为同一个事件原子持久化。
	evSwapAgreed      = "swap_agreed"
	evSwapRejected    = "swap_rejected"
	evSwapWithdrawn   = "swap_withdrawn"
	evSwapCanceled    = "swap_canceled"
	evSwapInvalidated = "swap_invalidated"
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

// StaffingRequirementSetPayload 设置或更新一个岗位时间段的最少人数。
type StaffingRequirementSetPayload struct {
	Requirement StaffingRequirement `json:"requirement"`
}

// ScheduleChangedPayload 表示排班版本推进。
type ScheduleChangedPayload struct {
	Version int64  `json:"version"`
	Reason  string `json:"reason"`
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

// SwapCanceledPayload 由管理员取消申请。
type SwapCanceledPayload struct {
	RequestID string `json:"requestId"`
	AdminID   string `json:"adminId"`
	Reason    string `json:"reason,omitempty"`
}

// SwapInvalidatedPayload 表示排班版本推进后旧申请自动失效。
type SwapInvalidatedPayload struct {
	RequestID       string `json:"requestId"`
	Reason          string `json:"reason"`
	ScheduleVersion int64  `json:"scheduleVersion"`
}

// Event 是持久化的最小单元。
type Event struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"ts"`
	Payload   map[string]any `json:"payload"`
}
