package goshiftexchange

import (
	"fmt"
	"sort"
	"time"
)

// overlaps 判断两个班次时间区间是否重叠；端点相接（a.End == b.Start）不算重叠。
func overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}

// validateShiftShape 校验单个班次自身的合法性。
func validateShiftShape(s Shift) error {
	var v []string
	if s.ID == "" {
		v = append(v, "shift id is required")
	}
	if s.Position == "" {
		v = append(v, "shift position is required")
	}
	if s.EmployeeID == "" {
		v = append(v, "shift employee id is required")
	}
	if !s.Start.IsZero() && !s.End.IsZero() && !s.End.After(s.Start) {
		v = append(v, fmt.Sprintf("shift %q end must be after start", s.ID))
	}
	return validationError(v)
}

// validateEmployeeSchedule 校验“换班后的完整排班”。
//
// shifts 为生效后所有相关班次的最终状态（未涉及班次保持现状，一并传入）。
// 校验不是逐条针对原班次，而是以员工为维度检查其完整班表：
//   - 岗位资质：员工必须持有每个所排班次岗位的资质；
//   - 时间不重叠：同一员工任意两个班次不得时间重叠；
//   - 最短休息间隔：同一员工相邻班次之间必须至少有 minRest 的间隔。
func validateEmployeeSchedule(
	employees map[string]Employee,
	shifts map[string]*Shift,
	minRest time.Duration,
) error {
	byEmployee := make(map[string][]*Shift)
	for _, sh := range shifts {
		byEmployee[sh.EmployeeID] = append(byEmployee[sh.EmployeeID], sh)
	}

	var violations []string
	for empID, list := range byEmployee {
		emp, ok := employees[empID]
		if !ok {
			violations = append(violations, fmt.Sprintf("employee %q not registered", empID))
			continue
		}
		qualified := make(map[string]bool, len(emp.Positions))
		for _, p := range emp.Positions {
			qualified[p] = true
		}

		// 先按开始时间排序，重叠与休息间隔都只需检查相邻班次；
		// 资质与岗位绑定，顺序无关，一并在同一轮中检查。
		sorted := append([]*Shift(nil), list...)
		sort.Slice(sorted, func(i, j int) bool {
			if !sorted[i].Start.Equal(sorted[j].Start) {
				return sorted[i].Start.Before(sorted[j].Start)
			}
			return sorted[i].ID < sorted[j].ID
		})

		for i, sh := range sorted {
			if !qualified[sh.Position] {
				violations = append(violations, fmt.Sprintf(
					"employee %q is not qualified for position %q (shift %q)",
					empID, sh.Position, sh.ID))
			}
			if i == 0 {
				continue
			}
			prev := sorted[i-1]
			if overlaps(prev.Start, prev.End, sh.Start, sh.End) {
				violations = append(violations, fmt.Sprintf(
					"employee %q has overlapping shifts %q and %q",
					empID, prev.ID, sh.ID))
			}
			gap := sh.Start.Sub(prev.End)
			if gap < minRest {
				violations = append(violations, fmt.Sprintf(
					"employee %q rest between shifts %q and %q is %s, less than minimum %s",
					empID, prev.ID, sh.ID, gap, minRest))
			}
		}
	}
	return validationError(violations)
}

// buildProspectiveSchedule 以当前 live 班次为基础，套用申请中的轮换关系，
// 返回换班生效后的完整班次副本。它只负责构造，不做任何合法性判断。
//
// currentHolder 同时返回每个涉及班次“此刻”的持有人，供调用方核对持有人与版本。
func buildProspectiveSchedule(
	live map[string]*Shift,
	rotations []Rotation,
) (map[string]*Shift, map[string]string) {
	prospective := make(map[string]*Shift, len(live))
	for id, sh := range live {
		cp := *sh
		prospective[id] = &cp
	}
	currentHolder := make(map[string]string, len(rotations))
	for _, r := range rotations {
		if sh, ok := prospective[r.ShiftID]; ok {
			currentHolder[r.ShiftID] = sh.EmployeeID
			sh.EmployeeID = r.NewEmployeeID
		}
	}
	return prospective, currentHolder
}
