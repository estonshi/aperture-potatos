package kernel_test

import (
	"reflect"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/schemadrift"
	"github.com/estonshi/aperture-potatos/kernel"
)

// TestSchemaDrift covers the kernel types outside hook I/O that a binding exchanges as JSON: tools, commands, tasks, approval rules and the call ledger.
func TestSchemaDrift(t *testing.T) {
	schemadrift.Extra(reflect.TypeFor[kernel.ToolSpec](), "timeout_ms")
	for typ, loc := range map[reflect.Type]string{
		reflect.TypeFor[kernel.ToolSpec]():        "tool.schema.json#/$defs/tool_spec",
		reflect.TypeFor[kernel.ToolResult]():      "tool.schema.json#/$defs/tool_result",
		reflect.TypeFor[kernel.Pin]():             "tool.schema.json#/$defs/pin",
		reflect.TypeFor[kernel.CommandResult]():   "command.schema.json#/$defs/command_result",
		reflect.TypeFor[kernel.Input]():           "command.schema.json#/$defs/input",
		reflect.TypeFor[kernel.CompactReport]():   "command.schema.json#/$defs/compact_report",
		reflect.TypeFor[kernel.UsageReport]():     "command.schema.json#/$defs/usage_report",
		reflect.TypeFor[kernel.TaskSpec]():        "task.schema.json#/$defs/task_spec",
		reflect.TypeFor[kernel.TaskInstruction](): "task.schema.json#/$defs/task_instruction",
		reflect.TypeFor[kernel.TaskProgress]():    "task.schema.json#/$defs/task_progress",
		reflect.TypeFor[kernel.TaskResult]():      "task.schema.json#/$defs/task_result",
		reflect.TypeFor[kernel.TaskInfo]():        "task.schema.json#/$defs/task_info",
		reflect.TypeFor[kernel.ApprovalRule]():    "approval_rule.schema.json#/$defs/rule",
		reflect.TypeFor[kernel.Condition]():       "approval_rule.schema.json#/$defs/condition",
		reflect.TypeFor[kernel.ModelCall]():       "common.schema.json#/$defs/model_call",
		reflect.TypeFor[kernel.TurnCalls]():       "common.schema.json#/$defs/turn_calls",
		reflect.TypeFor[kernel.TurnCost]():        "common.schema.json#/$defs/turn_cost",
		reflect.TypeFor[kernel.CallCost]():        "common.schema.json#/$defs/call_cost",
		reflect.TypeFor[kernel.Cost]():            "common.schema.json#/$defs/cost",
	} {
		schemadrift.Check(t, typ, loc)
	}
}
